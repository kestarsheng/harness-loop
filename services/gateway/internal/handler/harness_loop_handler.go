package handler

import (
	"context"
	"encoding/json"
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

func queryLimit(c *gin.Context, fallback int) int {
	if raw := c.Query("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return fallback
}

func (h *HarnessLoopHandler) ListSubtasks(c *gin.Context) {
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	resp, err := client.ListSubtasks(outboundContext(c), &harnesspb.ListSubtasksRequest{
		TenantId:  tenantOf(c),
		ProjectId: c.Query("project_id"),
		TaskId:    c.Param("id"),
		Limit:     int32(queryLimit(c, 200)),
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	subtasks := resp.Subtasks
	if subtasks == nil {
		subtasks = []*harnesspb.SubtaskView{}
	}
	c.JSON(http.StatusOK, gin.H{"subtasks": subtasks})
}

func (h *HarnessLoopHandler) ListModelSelections(c *gin.Context) {
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	resp, err := client.ListModelSelections(outboundContext(c), &harnesspb.ListModelSelectionsRequest{
		TenantId:  tenantOf(c),
		ProjectId: c.Query("project_id"),
		TaskId:    c.Param("id"),
		Stage:     c.Query("stage"),
		Limit:     int32(queryLimit(c, 200)),
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	selections := resp.Selections
	if selections == nil {
		selections = []*harnesspb.ModelSelectionView{}
	}
	c.JSON(http.StatusOK, gin.H{"selections": selections})
}

func (h *HarnessLoopHandler) ListHandoffs(c *gin.Context) {
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}
	resp, err := client.ListHandoffs(outboundContext(c), &harnesspb.ListHandoffsRequest{
		TenantId:  tenantOf(c),
		ProjectId: c.Query("project_id"),
		TaskId:    c.Query("task_id"),
		Limit:     int32(queryLimit(c, 200)),
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	handoffs := resp.Handoffs
	if handoffs == nil {
		handoffs = []*harnesspb.HandoffView{}
	}
	c.JSON(http.StatusOK, gin.H{"handoffs": handoffs})
}

// MonitorStream bridges the gRPC WatchEvents stream to SSE. EventSource cannot
// send custom headers, so the tenant is read from the query string and
// validated explicitly instead of relying on StrictTenant's X-Tenant-ID.
func (h *HarnessLoopHandler) MonitorStream(c *gin.Context) {
	tenantID := strings.TrimSpace(c.Query("tenant"))
	if tenantID == "" {
		writeEnvelope(c, http.StatusBadRequest, "E_TENANT_REQUIRED", "tenant query parameter is required", map[string]any{"parameter": "tenant"})
		return
	}
	lastSeq := int64(0)
	if raw := strings.TrimSpace(c.Query("last_seq")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeEnvelope(c, http.StatusBadRequest, "E_INVALID_ARGUMENT", "last_seq must be a non-negative integer", nil)
			return
		}
		lastSeq = v
	}
	client, err := h.ensureClient()
	if err != nil {
		writeRPCError(c, err)
		return
	}

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.Flush()

	// Catch-up phase: replay everything after last_seq before switching to
	// the live stream. ListEvents uses seq > since, so replays never repeat.
	since := lastSeq
	for {
		resp, listErr := client.ListEvents(ctx, &harnesspb.ListEventsRequest{
			TenantId: tenantID, SinceSeq: since, Limit: 500,
		})
		if listErr != nil {
			writeSSEMessage(c.Writer, "", "error", gin.H{"code": "E_CATCHUP_FAILED", "message": statusMessage(listErr)})
			return
		}
		for _, e := range resp.Events {
			if !writeSSEEvent(c.Writer, e) {
				return
			}
			if e.Seq > since {
				since = e.Seq
			}
		}
		if len(resp.Events) < 500 {
			break
		}
	}
	if !writeSSEMessage(c.Writer, "", "synced", gin.H{"last_seq": since}) {
		return
	}

	stream, streamErr := client.WatchEvents(ctx, &harnesspb.WatchEventsRequest{TenantId: tenantID, SinceSeq: since})
	if streamErr != nil {
		writeSSEMessage(c.Writer, "", "error", gin.H{"code": "E_WATCH_FAILED", "message": statusMessage(streamErr)})
		return
	}
	defer func() { _ = stream.CloseSend() }()

	type recvResult struct {
		event *harnesspb.HarnessEvent
		err   error
	}
	received := make(chan recvResult, 32)
	go func() {
		defer close(received)
		for {
			event, recvErr := stream.Recv()
			select {
			case received <- recvResult{event: event, err: recvErr}:
			case <-ctx.Done():
				return
			}
			if recvErr != nil {
				return
			}
		}
	}()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if !writeSSEMessage(c.Writer, "", "heartbeat", gin.H{"ts": time.Now().UTC().Format(time.RFC3339Nano)}) {
				return
			}
		case res, ok := <-received:
			if !ok {
				return
			}
			if res.err != nil {
				return
			}
			if res.event == nil {
				continue
			}
			if !writeSSEEvent(c.Writer, res.event) {
				return
			}
		}
	}
}

func writeSSEEvent(w gin.ResponseWriter, e *harnesspb.HarnessEvent) bool {
	payload := json.RawMessage(e.Payload)
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	return writeSSEMessage(w, e.EventId, "harness", gin.H{
		"event_id":       e.EventId,
		"event_type":     e.EventType,
		"schema_version": e.SchemaVersion,
		"task_id":        e.TaskId,
		"seq":            e.Seq,
		"timestamp_ms":   e.TimestampMs,
		"severity":       e.Severity,
		"tenant_id":      e.TenantId,
		"project_id":     e.ProjectId,
		"stage":          e.GetStage(),
		"attempt":        e.GetAttempt(),
		"trace_id":       e.TraceId,
		"span_id":        e.SpanId,
		"agent_id":       e.AgentId,
		"parent_task_id": e.ParentTaskId,
		"subtask_id":     e.SubtaskId,
		"handoff_id":     e.HandoffId,
		"dag_id":         e.DagId,
		"dag_node_id":    e.DagNodeId,
		"payload":        payload,
	})
}

func writeSSEMessage(w gin.ResponseWriter, id, event string, data any) bool {
	body, err := json.Marshal(data)
	if err != nil {
		return false
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return false
		}
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return false
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
		return false
	}
	w.Flush()
	return true
}
