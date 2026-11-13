package policy

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	key      string
	decision Decision
	expires  time.Time
	elem     *list.Element
}

type DecisionCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	entries map[string]*cacheEntry
	order   *list.List // front = most recently used
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
		order:   list.New(),
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
	c.order.MoveToFront(entry.elem)
	c.hits++
	return entry.decision, true
}

func (c *DecisionCache) Put(key string, d Decision) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[key]; ok {
		entry.decision = d
		entry.expires = c.now().Add(c.ttl)
		c.order.MoveToFront(entry.elem)
		return
	}
	for len(c.entries) >= c.max && c.order.Len() > 0 {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.removeLocked(oldest.Value.(*cacheEntry).key)
	}
	entry := &cacheEntry{key: key, decision: d, expires: c.now().Add(c.ttl)}
	entry.elem = c.order.PushFront(entry)
	c.entries[key] = entry
}

func (c *DecisionCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*cacheEntry, c.max)
	c.order.Init()
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

func (c *DecisionCache) removeLocked(key string) {
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	if entry.elem != nil {
		c.order.Remove(entry.elem)
	}
	delete(c.entries, key)
}

// DecisionKey builds the cache key with one SHA-256 pass over a length-prefixed
// encoding of every input. It avoids the previous pre-hashing plus JSON policy
// fingerprint, both of which cost more than the decision itself on the hot path.
func DecisionKey(req Request, policyVersion, approvalVersion int64) string {
	buf := make([]byte, 0, 256)
	buf = appendKeyString(buf, req.TenantID)
	buf = appendKeyString(buf, req.ProjectID)
	buf = appendKeyString(buf, req.TaskID)
	buf = appendKeyString(buf, req.SubtaskID)
	buf = appendKeyString(buf, req.Stage)
	buf = appendKeyString(buf, req.RoleName)
	buf = appendKeyString(buf, req.Tool)
	buf = appendKeyStrings(buf, req.Args.Paths)
	buf = appendKeyInt(buf, policyVersion)
	buf = appendKeyInt(buf, approvalVersion)
	buf = appendKeyArgs(buf, req.Args)
	buf = appendKeyPolicySet(buf, req.Tenant)
	buf = appendKeyPolicySet(buf, req.Role)
	buf = appendKeyPolicySet(buf, req.Task)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

func appendKeyString(buf []byte, s string) []byte {
	buf = binary.BigEndian.AppendUint64(buf, uint64(len(s)))
	return append(buf, s...)
}

func appendKeyStrings(buf []byte, list []string) []byte {
	buf = appendKeyInt(buf, int64(len(list)))
	for _, item := range list {
		buf = appendKeyString(buf, item)
	}
	return buf
}

func appendKeyInt(buf []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(buf, uint64(v))
}

func appendKeyBool(buf []byte, v bool) []byte {
	if v {
		return append(buf, 1)
	}
	return append(buf, 0)
}

func appendKeyArgs(buf []byte, args ToolArgs) []byte {
	buf = appendKeyStrings(buf, args.Command)
	buf = appendKeyString(buf, args.NetworkMethod)
	buf = appendKeyString(buf, strconv.FormatFloat(args.EstimatedCost, 'f', 6, 64))
	return buf
}

func appendKeyPolicySet(buf []byte, set *PolicySet) []byte {
	if set == nil {
		return appendKeyBool(buf, false)
	}
	buf = appendKeyBool(buf, true)
	buf = appendKeyStrings(buf, set.AllowTools)
	buf = appendKeyStrings(buf, set.DenyTools)
	buf = appendKeyInt(buf, int64(len(set.Rules)))
	for _, rule := range set.Rules {
		buf = appendKeyString(buf, rule.ID)
		buf = appendKeyString(buf, rule.Tool)
		buf = appendKeyStrings(buf, rule.Tags)
		buf = appendKeyStrings(buf, rule.Stages)
		buf = appendKeyString(buf, string(rule.Effect))
		buf = appendKeyBool(buf, rule.RequiresApproval)
	}
	return buf
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