package handler

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"agent-platform/services/gateway/internal/middleware"
)

// Audit actions covered by the gateway. They map 1:1 to the rules declared in
// the router so the audit trail stays a closed set instead of free-form text.
const (
	AuditActionStart        = "start"
	AuditActionPause        = "pause"
	AuditActionResume       = "resume"
	AuditActionAbort        = "abort"
	AuditActionRebuild      = "rebuild"
	AuditActionDelete       = "delete"
	AuditActionConfigChange = "config_change"
)

// AuditEntry is the retained form of an audit record. There is intentionally no
// mutation or deletion path: the log is append-only from the gateway's view.
type AuditEntry struct {
	ID        string    `json:"id"`
	Operator  string    `json:"operator"`
	Time      time.Time `json:"time"`
	TenantID  string    `json:"tenant_id"`
	Action    string    `json:"action"`
	Object    string    `json:"object"`
	Result    string    `json:"result"`
	SourceIP  string    `json:"source_ip"`
	TraceID   string    `json:"trace_id"`
	Detail    string    `json:"detail"`
	Confirmed string    `json:"confirmed"`
}

// AuditLog is an append-only, tenant-scoped audit store. It satisfies
// middleware.AuditSink and is safe for concurrent use.
type AuditLog struct {
	mu      sync.RWMutex
	entries []AuditEntry
	max     int
	seq     atomic.Uint64
}

// NewAuditLog creates a ring buffer capped at max entries. Non-positive max
// falls back to the default capacity.
func NewAuditLog(max int) *AuditLog {
	if max <= 0 {
		max = 10000
	}
	return &AuditLog{max: max}
}

// Record appends one audit record. Older entries are dropped once the ring is
// full; the structured gateway log keeps the long-term copy.
func (l *AuditLog) Record(rec middleware.AuditRecord) {
	if l == nil {
		return
	}
	entry := AuditEntry{
		ID:        "audit-" + strconv.FormatUint(l.seq.Add(1), 10),
		Operator:  rec.Operator,
		Time:      time.Now().UTC(),
		TenantID:  rec.TenantID,
		Action:    rec.Action,
		Object:    rec.Object,
		Result:    rec.Result,
		SourceIP:  rec.SourceIP,
		TraceID:   rec.TraceID,
		Detail:    rec.Detail,
		Confirmed: rec.Confirmed,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
}

// List returns the audit entries visible to tenantID, newest first.
func (l *AuditLog) List(tenantID, action, object string, limit int) []AuditEntry {
	if l == nil {
		return nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]AuditEntry, 0, limit)
	for i := len(l.entries) - 1; i >= 0; i-- {
		entry := l.entries[i]
		if entry.TenantID != tenantID {
			continue
		}
		if action != "" && entry.Action != action {
			continue
		}
		if object != "" && entry.Object != object {
			continue
		}
		out = append(out, entry)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Count returns the number of retained entries for a tenant.
func (l *AuditLog) Count(tenantID string) int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := 0
	for _, entry := range l.entries {
		if entry.TenantID == tenantID {
			n++
		}
	}
	return n
}

// ListAudit returns the calling tenant's audit trail. There is no cross-tenant
// read path: the tenant always comes from the request context.
func (h *HarnessLoopHandler) ListAudit(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	tenantID := tenantOf(c)
	entries := h.audit.List(tenantID, c.Query("action"), c.Query("object"), limit)
	c.JSON(http.StatusOK, gin.H{
		"entries": entries,
		"total":   h.audit.Count(tenantID),
	})
}

// DeleteAudit always fails for ordinary users: the audit trail is immutable.
// The attempt itself is recorded so tampering shows up in the trail.
func (h *HarnessLoopHandler) DeleteAudit(c *gin.Context) {
	h.audit.Record(middleware.AuditRecord{
		Operator: "anonymous",
		TenantID: tenantOf(c),
		Action:   AuditActionDelete,
		Object:   "audit:*",
		Result:   "denied",
		SourceIP: c.ClientIP(),
		TraceID:  c.GetHeader("X-Trace-ID"),
		Detail:   "audit records are immutable",
	})
	writeEnvelope(c, http.StatusForbidden, "E_AUDIT_IMMUTABLE",
		"audit records cannot be deleted by ordinary users",
		map[string]any{"action": AuditActionDelete})
}

// AuditRules declares every auditable operation. Paths use gin's registered
// pattern so the middleware can match on c.FullPath(). Destructive legacy
// routes accept an explicit confirm=false as a rejection but keep working when
// the client sends no confirm field at all.
func AuditRules() []middleware.AuditRule {
	const loop = "/api/v2/harness-loop"
	return []middleware.AuditRule{
		{Method: "POST", Path: loop + "/tasks", Action: AuditActionStart, ObjectKind: "task"},
		{Method: "POST", Path: loop + "/tasks/:id/pause", Action: AuditActionPause, ObjectKind: "task", ObjectParam: "id", ConfirmAllowed: true},
		{Method: "POST", Path: loop + "/tasks/:id/resume", Action: AuditActionResume, ObjectKind: "task", ObjectParam: "id"},
		{Method: "POST", Path: loop + "/tasks/:id/abort", Action: AuditActionAbort, ObjectKind: "task", ObjectParam: "id", ConfirmAllowed: true},
		{Method: "POST", Path: loop + "/checkpoints/restore", Action: AuditActionRebuild, ObjectKind: "checkpoint"},
		{Method: "DELETE", Path: "/api/v2/memory/session/:id", Action: AuditActionDelete, ObjectKind: "memory-session", ObjectParam: "id", ConfirmAllowed: true},
		{Method: "DELETE", Path: "/api/v2/memory/session/clear", Action: AuditActionDelete, ObjectKind: "memory-session", ConfirmAllowed: true},
		{Method: "DELETE", Path: "/api/v2/memory/:id", Action: AuditActionDelete, ObjectKind: "memory", ObjectParam: "id", ConfirmAllowed: true},
		{Method: "DELETE", Path: "/api/v2/knowledge/documents/:id", Action: AuditActionDelete, ObjectKind: "knowledge-document", ObjectParam: "id", ConfirmAllowed: true},
		{Method: "POST", Path: "/api/v2/harness/gateway/config", Action: AuditActionConfigChange, ObjectKind: "gateway-config", ConfirmAllowed: true},
		{Method: "PUT", Path: "/api/v2/harness/gateway/config/:id", Action: AuditActionConfigChange, ObjectKind: "gateway-config", ObjectParam: "id", ConfirmAllowed: true},
		{Method: "DELETE", Path: "/api/v2/harness/gateway/config/:id", Action: AuditActionDelete, ObjectKind: "gateway-config", ObjectParam: "id", ConfirmAllowed: true},
		{Method: "PUT", Path: "/api/v2/memory/forgetting/config", Action: AuditActionConfigChange, ObjectKind: "forgetting-config", ConfirmAllowed: true},
	}
}
