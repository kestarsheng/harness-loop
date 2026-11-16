// Package middleware provides HTTP middleware
package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"agent-platform/pkg/redis"

	"github.com/gin-gonic/gin"
)

// Logger returns a logger middleware
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		gin.DefaultWriter.Write([]byte(
			time.Now().Format("2006/01/02 - 15:04:05") +
				" | " + c.Request.Method +
				" | " + path +
				" | " + time.Duration(latency).String() +
				" | " + strconv.Itoa(status) + "\n",
		))
	}
}

// CORS returns a CORS middleware
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Tenant-ID")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// Tenant extracts tenant ID from header
func Tenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.GetHeader("X-Tenant-ID")
		if tenantID == "" {
			tenantID = "default"
		}
		c.Set("tenant_id", tenantID)
		c.Next()
	}
}

// RateLimit returns a rate limiting middleware
func RateLimit(redisClient *redis.Client, limit int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, exists := c.Get("tenant_id")
		if !exists {
			tenantID = "default"
		}

		tenantIDStr, ok := tenantID.(string)
		if !ok {
			tenantIDStr = "default"
		}

		key := "ratelimit:" + tenantIDStr + ":" + c.FullPath()

		allowed, remaining, err := redisClient.RateLimit(c.Request.Context(), key, limit, window)
		if err != nil {
			c.Next()
			return
		}

		c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", limit))
		c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))

		if !allowed {
			c.JSON(429, gin.H{
				"code":    20004,
				"message": "rate limit exceeded",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// Auth returns an authentication middleware
func Auth(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if token == "" {
			c.JSON(401, gin.H{
				"code":    10002,
				"message": "unauthorized",
			})
			c.Abort()
			return
		}

		// TODO: Validate JWT token
		// For now, just pass through
		c.Next()
	}
}

// StrictTenant requires an explicit X-Tenant-ID header and rejects requests without it.
// The namespace is only used in the error envelope, so callers can configure which
// namespaces are guarded without touching this middleware.
func StrictTenant(namespaces ...string) gin.HandlerFunc {
	namespace := "harness-loop"
	if len(namespaces) > 0 && namespaces[0] != "" {
		namespace = namespaces[0]
	}
	return func(c *gin.Context) {
		tenantID := c.GetHeader("X-Tenant-ID")
		if tenantID == "" {
			c.AbortWithStatusJSON(401, gin.H{
				"code":     "E_TENANT_REQUIRED",
				"message":  "X-Tenant-ID header is required for this namespace",
				"details":  gin.H{"namespace": namespace},
				"trace_id": c.GetHeader("X-Trace-ID"),
			})
			return
		}
		c.Set("tenant_id", tenantID)
		c.Next()
	}
}

// StrictTenantForPaths guards every request whose path starts with one of the
// configured prefixes. Prefixes prefixed with "!" are exceptions (SSE endpoints
// cannot set headers, so the default list excludes the monitor stream).
func StrictTenantForPaths(prefixes []string) gin.HandlerFunc {
	if len(prefixes) == 0 {
		prefixes = DefaultStrictTenantPaths()
	}
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		guarded := false
		namespace := ""
		for _, prefix := range prefixes {
			if strings.HasPrefix(prefix, "!") {
				if strings.HasPrefix(path, strings.TrimPrefix(prefix, "!")) {
					c.Next()
					return
				}
				continue
			}
			if prefix != "" && strings.HasPrefix(path, prefix) {
				guarded = true
				namespace = strings.TrimPrefix(prefix, "/")
			}
		}
		if !guarded {
			c.Next()
			return
		}
		StrictTenant(namespace)(c)
	}
}

// DefaultStrictTenantPaths returns the namespaces that require an explicit tenant
// header when no configuration is supplied. Only harness-loop is strict today;
// the SSE monitor stream is excluded because EventSource cannot set X-Tenant-ID.
func DefaultStrictTenantPaths() []string {
	return []string{"/api/v2/harness-loop", "!/api/v2/harness-loop/monitor/stream"}
}

// AuditRule describes one auditable operation. Path must match gin's FullPath
// (the registered pattern, e.g. /api/v2/harness-loop/tasks/:id/pause).
type AuditRule struct {
	Method     string
	Path       string
	Action     string
	ObjectKind string
	// ObjectParam names the path parameter holding the object id. Empty falls
	// back to the first non-empty path parameter.
	ObjectParam string
	// ConfirmAllowed accepts an explicit confirm=false rejection without
	// breaking legacy clients that send no confirm field at all.
	ConfirmAllowed bool
}

// AuditRecord is the normalized audit entry produced by the Audit middleware.
type AuditRecord struct {
	Operator  string
	TenantID  string
	Action    string
	Object    string
	Result    string
	SourceIP  string
	TraceID   string
	Detail    string
	Confirmed string
}

// AuditSink receives audit records. Implementations must never expose deletion.
type AuditSink interface {
	Record(rec AuditRecord)
}

// Audit records destructive and lifecycle operations. The middleware only
// inspects requests matching the supplied rules, so unrelated legacy routes keep
// their current behaviour.
func Audit(sink AuditSink, rules []AuditRule) gin.HandlerFunc {
	index := make(map[string]AuditRule, len(rules))
	for _, r := range rules {
		index[r.Method+" "+r.Path] = r
	}
	return func(c *gin.Context) {
		rule, ok := index[c.Request.Method+" "+c.FullPath()]
		if !ok {
			c.Next()
			return
		}
		confirmed := "implicit"
		if rule.ConfirmAllowed {
			body, err := c.GetRawData()
			if err == nil && len(body) > 0 {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
				var probe struct {
					Confirm *bool `json:"confirm"`
				}
				if json.Unmarshal(body, &probe) == nil && probe.Confirm != nil {
					if !*probe.Confirm {
						c.AbortWithStatusJSON(400, gin.H{
							"code":     "E_CONFIRMATION_REQUIRED",
							"message":  "confirm=true is required for this operation",
							"details":  gin.H{"action": rule.Action},
							"trace_id": c.GetHeader("X-Trace-ID"),
						})
						sink.Record(AuditRecord{
							Operator:  operatorOf(c),
							TenantID:  tenantOf(c),
							Action:    rule.Action,
							Object:    objectOf(c, rule),
							Result:    "denied",
							SourceIP:  c.ClientIP(),
							TraceID:   c.GetHeader("X-Trace-ID"),
							Detail:    "confirmation rejected by client",
							Confirmed: "false",
						})
						return
					}
					confirmed = "true"
				}
			}
		}

		c.Next()

		result := "success"
		switch status := c.Writer.Status(); {
		case status >= 200 && status < 300:
			result = "success"
		case status == 401 || status == 403 || status == 404:
			result = "denied"
		default:
			result = "failure"
		}
		sink.Record(AuditRecord{
			Operator:  operatorOf(c),
			TenantID:  tenantOf(c),
			Action:    rule.Action,
			Object:    objectOf(c, rule),
			Result:    result,
			SourceIP:  c.ClientIP(),
			TraceID:   c.GetHeader("X-Trace-ID"),
			Detail:    "http_status=" + strconv.Itoa(c.Writer.Status()),
			Confirmed: confirmed,
		})
	}
}

func tenantOf(c *gin.Context) string {
	if v := c.GetString("tenant_id"); v != "" {
		return v
	}
	if v := c.GetHeader("X-Tenant-ID"); v != "" {
		return v
	}
	return "default"
}

func operatorOf(c *gin.Context) string {
	for _, header := range []string{"X-User-ID", "X-Operator"} {
		if v := c.GetHeader(header); v != "" {
			return v
		}
	}
	return "anonymous"
}

func objectOf(c *gin.Context, rule AuditRule) string {
	kind := rule.ObjectKind
	if kind == "" {
		kind = "object"
	}
	id := ""
	if rule.ObjectParam != "" {
		id = c.Param(rule.ObjectParam)
	} else {
		for _, param := range c.Params {
			if param.Value != "" {
				id = param.Value
				break
			}
		}
	}
	if id == "" {
		return kind + ":*"
	}
	return kind + ":" + id
}
