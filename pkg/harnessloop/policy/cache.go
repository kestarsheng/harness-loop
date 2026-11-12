package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	decision Decision
	expires  time.Time
}

type DecisionCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	entries map[string]*cacheEntry
	order   []string
	hits    int64
	misses  int64
}

func NewDecisionCache(max int, ttl time.Duration) *DecisionCache {
	if max <= 0 {
		max = 10000
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &DecisionCache{
		max:     max,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]*cacheEntry, max),
	}
}

func (c *DecisionCache) SetClock(now func() time.Time) {
	if now != nil {
		c.now = now
	}
}

func (c *DecisionCache) Get(key string) (Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		c.misses++
		return Decision{}, false
	}
	if !c.now().Before(entry.expires) {
		c.removeLocked(key)
		c.misses++
		return Decision{}, false
	}
	c.touchLocked(key)
	c.hits++
	return entry.decision, true
}

func (c *DecisionCache) Put(key string, d Decision) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.entries[key]; ok {
		c.removeLocked(key)
	}
	for len(c.entries) >= c.max && len(c.order) > 0 {
		c.removeLocked(c.order[0])
	}
	c.entries[key] = &cacheEntry{decision: d, expires: c.now().Add(c.ttl)}
	c.order = append(c.order, key)
}

func (c *DecisionCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*cacheEntry, c.max)
	c.order = nil
}

func (c *DecisionCache) Stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

func (c *DecisionCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *DecisionCache) touchLocked(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), key)
			return
		}
	}
}

func (c *DecisionCache) removeLocked(key string) {
	delete(c.entries, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func DecisionKey(req Request, policyVersion, approvalVersion int64) string {
	parts := []string{
		req.TenantID,
		req.ProjectID,
		req.TaskID,
		req.SubtaskID,
		req.Stage,
		req.RoleName,
		req.Tool,
		strings.Join(req.Args.Paths, "\x00"),
		strconv.FormatInt(policyVersion, 10),
		strconv.FormatInt(approvalVersion, 10),
		argsDigest(req.Args),
		policyFingerprint(req),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func argsDigest(args ToolArgs) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.Join(args.Paths, "\x00"),
		strings.Join(args.Command, "\x00"),
		args.NetworkMethod,
		strconv.FormatFloat(args.EstimatedCost, 'f', 6, 64),
	}, "\x1f")))
	return hex.EncodeToString(sum[:])
}
func policyFingerprint(req Request) string {
	raw, err := json.Marshal(struct {
		Tenant *PolicySet `json:"tenant"`
		Role   *PolicySet `json:"role"`
		Task   *PolicySet `json:"task"`
	}{req.Tenant, req.Role, req.Task})
	if err != nil {
		return "unavailable"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
