package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"agent-platform/pkg/config"
	harnesspb "agent-platform/pkg/pb/harnessloop"
)

const harnessLoopNamespace = "harness-loop"

type HarnessLoopHandler struct {
	cfg *config.Config

	mu     sync.Mutex
	conn   *grpc.ClientConn
	client harnesspb.HarnessLoopServiceClient
}

func NewHarnessLoopHandler(cfg *config.Config) *HarnessLoopHandler {
	return &HarnessLoopHandler{cfg: cfg}
}

func (h *HarnessLoopHandler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != nil {
		_ = h.conn.Close()
		h.conn = nil
		h.client = nil
	}
}

func (h *HarnessLoopHandler) ensureClient() (harnesspb.HarnessLoopServiceClient, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		return h.client, nil
	}
	target := ""
	if h.cfg != nil {
		target = h.cfg.Services.HarnessLoop
	}
	if target == "" {
		return nil, status.Error(codes.Unavailable, "E_HARNESS_LOOP_NOT_CONFIGURED: services.harness_loop is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "E_HARNESS_LOOP_UNAVAILABLE: %v", err)
	}
	h.conn = conn
	h.client = harnesspb.NewHarnessLoopServiceClient(conn)
	return h.client, nil
}

func outboundContext(c *gin.Context) context.Context {
	ctx := c.Request.Context()
	pairs := []string{}
	if traceID := c.GetHeader("X-Trace-ID"); traceID != "" {
		pairs = append(pairs, "x-trace-id", traceID)
	}
	if key := c.GetHeader("Idempotency-Key"); key != "" {
		pairs = append(pairs, "x-idempotency-key", key)
	}
	if len(pairs) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}

func tenantOf(c *gin.Context) string {
	return c.GetString("tenant_id")
}

func writeEnvelope(c *gin.Context, httpStatus int, code, message string, details map[string]any) {
	traceID := c.GetHeader("X-Trace-ID")
	if traceID == "" {
		traceID = c.GetString("trace_id")
	}
	if details == nil {
		details = map[string]any{}
	}
	c.JSON(httpStatus, gin.H{
		"code":     code,
		"message":  message,
		"details":  details,
		"trace_id": traceID,
	})
}

func writeRPCError(c *gin.Context, err error) {
	httpStatus, code := mapStatus(err)
	writeEnvelope(c, httpStatus, code, statusMessage(err), map[string]any{"upstream_code": grpcCodeString(err)})
}

func statusMessage(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

func grpcCodeString(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Code().String()
	}
	return "Unknown"
}

func mapStatus(err error) (int, string) {
	st, ok := status.FromError(err)
	if !ok {
		return http.StatusInternalServerError, "E_INTERNAL"
	}
	msg := st.Message()
	switch st.Code() {
	case codes.NotFound:
		return http.StatusNotFound, "E_NOT_FOUND"
	case codes.InvalidArgument:
		return http.StatusBadRequest, "E_INVALID_ARGUMENT"
	case codes.Aborted:
		return http.StatusConflict, "E_VERSION_CONFLICT"
	case codes.AlreadyExists:
		return http.StatusConflict, "E_ALREADY_EXISTS"
	case codes.PermissionDenied:
		return http.StatusForbidden, "E_FORBIDDEN"
	case codes.Unauthenticated:
		return http.StatusUnauthorized, "E_UNAUTHENTICATED"
	case codes.Unavailable:
		return http.StatusServiceUnavailable, "E_SERVICE_UNAVAILABLE"
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, "E_TIMEOUT"
	case codes.FailedPrecondition:
		switch {
		case strings.Contains(msg, "E_ILLEGAL_TRANSITION"):
			return http.StatusConflict, "E_ILLEGAL_TRANSITION"
		case strings.Contains(msg, "E_ACCEPTANCE_CONTEXT_INCOMPLETE"):
			return http.StatusUnprocessableEntity, "E_ACCEPTANCE_CONTEXT_INCOMPLETE"
		case strings.Contains(msg, "E_AGGREGATION_MISSING"):
			return http.StatusUnprocessableEntity, "E_AGGREGATION_MISSING"
		case strings.Contains(msg, "E_FSM_DISABLED"):
			return http.StatusServiceUnavailable, "E_FSM_DISABLED"
		}
		return http.StatusPreconditionFailed, "E_PRECONDITION_FAILED"
	}
	return http.StatusInternalServerError, "E_INTERNAL"
}

func (h *HarnessLoopHandler) ListTasks(c *gin.Context) {
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		if v, convErr := strconv.Atoi(raw); convErr == nil {
			limit = v
		}
	}
	resp, err := client.ListTasks(outboundContext(c), &harnesspb.ListTasksRequest{
		TenantId:  tenantOf(c),
		ProjectId: c.Query("project_id"),
		Status:    c.QueryArray("status"),
		Limit:     int32(limit),
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	tasks := resp.Tasks
	if tasks == nil {
		tasks = []*harnesspb.TaskView{}
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

func (h *HarnessLoopHandler) GetTask(c *gin.Context) {
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	task, err := client.GetTask(outboundContext(c), &harnesspb.GetTaskRequest{TenantId: tenantOf(c), TaskId: c.Param("id")})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

type createTaskBody struct {
	ProjectID     string `json:"project_id"`
	WorkspaceRoot string `json:"workspace_root"`
	Goal          string `json:"goal"`
	Budget        struct {
		Tokens int64   `json:"tokens"`
		Cost   float64 `json:"cost"`
	} `json:"budget"`
}

func (h *HarnessLoopHandler) CreateTask(c *gin.Context) {
	var body createTaskBody
	if err := c.ShouldBindJSON(&body); err != nil {
		writeEnvelope(c, http.StatusBadRequest, "E_INVALID_BODY", "request body is not valid json", nil)
		return
	}
	if body.Goal == "" || body.WorkspaceRoot == "" {
		writeEnvelope(c, http.StatusBadRequest, "E_INVALID_ARGUMENT", "goal and workspace_root are required", nil)
		return
	}
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	resp, err := client.CreateTask(outboundContext(c), &harnesspb.CreateTaskRequest{
		TenantId:      tenantOf(c),
		ProjectId:     body.ProjectID,
		WorkspaceRoot: body.WorkspaceRoot,
		Goal:          body.Goal,
		Budget:        &harnesspb.Budget{Tokens: body.Budget.Tokens, Cost: body.Budget.Cost},
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp.Task)
}

type actionBody struct {
	Confirm    bool   `json:"confirm"`
	Reason     string `json:"reason"`
	AbortEpoch int64  `json:"abort_epoch"`
}

func bindConfirm(c *gin.Context) (actionBody, bool) {
	var body actionBody
	if err := c.ShouldBindJSON(&body); err != nil {
		writeEnvelope(c, http.StatusBadRequest, "E_INVALID_BODY", "request body is not valid json", nil)
		return body, false
	}
	if !body.Confirm {
		writeEnvelope(c, http.StatusBadRequest, "E_CONFIRMATION_REQUIRED", "confirm=true is required for this operation", nil)
		return body, false
	}
	return body, true
}

func (h *HarnessLoopHandler) PauseTask(c *gin.Context) {
	body, ok := bindConfirm(c)
	if !ok {
		return
	}
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	task, err := client.PauseTask(outboundContext(c), &harnesspb.TaskCommand{
		TenantId: tenantOf(c), TaskId: c.Param("id"), Reason: body.Reason,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *HarnessLoopHandler) ResumeTask(c *gin.Context) {
	var body struct {
		CheckpointID string `json:"checkpoint_id"`
	}
	_ = c.ShouldBindJSON(&body)
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	task, err := client.ResumeTask(outboundContext(c), &harnesspb.ResumeTaskRequest{
		TenantId: tenantOf(c), TaskId: c.Param("id"), CheckpointId: body.CheckpointID,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *HarnessLoopHandler) AbortTask(c *gin.Context) {
	body, ok := bindConfirm(c)
	if !ok {
		return
	}
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	task, err := client.AbortTask(outboundContext(c), &harnesspb.TaskCommand{
		TenantId: tenantOf(c), TaskId: c.Param("id"), Reason: body.Reason, AbortEpoch: body.AbortEpoch,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *HarnessLoopHandler) ListEvents(c *gin.Context) {
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	sinceSeq := int64(0)
	if raw := c.Query("since_seq"); raw != "" {
		if v, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			sinceSeq = v
		}
	}
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		if v, convErr := strconv.Atoi(raw); convErr == nil {
			limit = v
		}
	}
	resp, err := client.ListEvents(outboundContext(c), &harnesspb.ListEventsRequest{
		TenantId:  tenantOf(c),
		TaskId:    c.Query("task_id"),
		Stage:     c.Query("stage"),
		EventType: c.Query("event_type"),
		Severity:  c.Query("severity"),
		SinceSeq:  sinceSeq,
		Limit:     int32(limit),
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	events := resp.Events
	if events == nil {
		events = []*harnesspb.HarnessEvent{}
	}
	c.JSON(http.StatusOK, gin.H{"events": events, "next_since_seq": resp.NextSinceSeq})
}

func (h *HarnessLoopHandler) ListCheckpoints(c *gin.Context) {
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	resp, err := client.ListCheckpoints(outboundContext(c), &harnesspb.ListCheckpointsRequest{
		TenantId: tenantOf(c), TaskId: c.Query("task_id"), Limit: 50,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	checkpoints := resp.Checkpoints
	if checkpoints == nil {
		checkpoints = []*harnesspb.CheckpointView{}
	}
	c.JSON(http.StatusOK, gin.H{"checkpoints": checkpoints})
}

func (h *HarnessLoopHandler) RestoreCheckpoint(c *gin.Context) {
	var body struct {
		TaskID       string `json:"task_id"`
		CheckpointID string `json:"checkpoint_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.TaskID == "" || body.CheckpointID == "" {
		writeEnvelope(c, http.StatusBadRequest, "E_INVALID_ARGUMENT", "task_id and checkpoint_id are required", nil)
		return
	}
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	task, err := client.RestoreFromCheckpoint(outboundContext(c), &harnesspb.RestoreRequest{
		TenantId: tenantOf(c), TaskId: body.TaskID, CheckpointId: body.CheckpointID,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *HarnessLoopHandler) GetReport(c *gin.Context) {
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	ctx := outboundContext(c)
	taskID := c.Param("id")
	tenantID := tenantOf(c)

	task, err := client.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: tenantID, TaskId: taskID})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	events, eventsErr := client.ListEvents(ctx, &harnesspb.ListEventsRequest{TenantId: tenantID, TaskId: taskID, Limit: 200})
	checkpoints, cpErr := client.ListCheckpoints(ctx, &harnesspb.ListCheckpointsRequest{TenantId: tenantID, TaskId: taskID, Limit: 20})

	report := gin.H{"task": task}
	if eventsErr == nil {
		report["events"] = events.Events
	}
	if cpErr == nil {
		report["checkpoints"] = checkpoints.Checkpoints
	}
	c.JSON(http.StatusOK, report)
}

var errHarnessLoopNotConfigured = errors.New("harness loop service is not configured")

func harnessLoopTarget(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Services.HarnessLoop
}

func describeTarget(cfg *config.Config) string {
	target := harnessLoopTarget(cfg)
	if target == "" {
		return fmt.Sprintf("<%v>", errHarnessLoopNotConfigured)
	}
	return target
}
