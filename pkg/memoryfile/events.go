package memoryfile

import (
	"errors"
	"time"
)

const (
	EventWritten      = "memory.written"
	EventMerged       = "memory.merged"
	EventEvicted      = "memory.evicted"
	EventPromoted     = "memory.lru_promoted"
	EventCorrupted    = "memory.corrupted"
	EventResync       = "memory.resync"
	EventCapacityWarn = "memory.capacity_warning"
)

var (
	ErrLockTimeout            = errors.New("memoryfile: lock acquisition timed out")
	ErrConcurrentModification = errors.New("memoryfile: file changed on disk during write")
	ErrCorrupted              = errors.New("memoryfile: memory file is corrupted")
	ErrWriteBusy              = errors.New("memoryfile: target file is busy")
)

type TopicChange struct {
	Key         string `json:"topic_key"`
	Action      string `json:"action"`
	ContentHash string `json:"content_hash,omitempty"`
	OrderRank   int    `json:"order_rank,omitempty"`
}

type ChangeEvent struct {
	Type         string        `json:"event_type"`
	File         string        `json:"file"`
	Scope        string        `json:"scope"`
	Topics       []TopicChange `json:"topics,omitempty"`
	OrderVersion int           `json:"order_version,omitempty"`
	Source       string        `json:"source,omitempty"`
	BackupPath   string        `json:"backup_path,omitempty"`
	Retained     int           `json:"retained_topics,omitempty"`
	Detail       string        `json:"detail,omitempty"`
	Timestamp    time.Time     `json:"timestamp"`
}

type EventSink interface {
	Emit(ChangeEvent)
}

type TopicAction string

const (
	ActionCreated  TopicAction = "created"
	ActionUpdated  TopicAction = "updated"
	ActionDeleted  TopicAction = "deleted"
	ActionEvicted  TopicAction = "evicted"
	ActionPromoted TopicAction = "promoted"
)
