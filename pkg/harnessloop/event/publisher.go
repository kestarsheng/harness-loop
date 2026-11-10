package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

type Sink interface {
	Append(ctx context.Context, events []*Event) error
}

type Buffer interface {
	Add(e *Event) error
	Drain() ([]*Event, error)
	Len() int
}

type MemoryBuffer struct {
	mu     sync.Mutex
	events []*Event
}

func NewMemoryBuffer() *MemoryBuffer { return &MemoryBuffer{} }

func (b *MemoryBuffer) Add(e *Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e.Clone())
	return nil
}

func (b *MemoryBuffer) Drain() ([]*Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.events
	b.events = nil
	return out, nil
}

func (b *MemoryBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

type JSONLBuffer struct {
	mu   sync.Mutex
	path string
}

func NewJSONLBuffer(path string) *JSONLBuffer { return &JSONLBuffer{path: path} }

func (b *JSONLBuffer) Add(e *Event) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("event buffer: marshal: %w", err)
	}
	raw = append(raw, '\n')
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.OpenFile(b.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("event buffer: open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("event buffer: write: %w", err)
	}
	return nil
}

func (b *JSONLBuffer) Drain() ([]*Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := os.ReadFile(b.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("event buffer: read: %w", err)
	}
	var out []*Event
	for len(raw) > 0 {
		idx := 0
		for idx < len(raw) && raw[idx] != '\n' {
			idx++
		}
		if idx == len(raw) {
			break
		}
		line := raw[:idx]
		raw = raw[idx+1:]
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, &e)
	}
	if err := os.Truncate(b.path, 0); err != nil {
		return nil, fmt.Errorf("event buffer: truncate: %w", err)
	}
	return out, nil
}

func (b *JSONLBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range raw {
		if c == '\n' {
			n++
		}
	}
	return n
}

type Publisher struct {
	mu     sync.Mutex
	sink   Sink
	idem   Idempotency
	buffer Buffer
	seqs   map[string]*SeqAllocator
}

func NewPublisher(sink Sink, idem Idempotency, buffer Buffer) *Publisher {
	return &Publisher{sink: sink, idem: idem, buffer: buffer, seqs: make(map[string]*SeqAllocator)}
}

func (p *Publisher) RestoreSeq(taskID string, watermark int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seqs[taskID] = NewSeqAllocator(watermark)
}

func (p *Publisher) nextSeq(taskID string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	alloc := p.seqs[taskID]
	if alloc == nil {
		alloc = NewSeqAllocator(0)
		p.seqs[taskID] = alloc
	}
	return alloc.Next()
}

func (p *Publisher) Publish(ctx context.Context, e *Event) error {
	if e == nil {
		return errors.New("event is nil")
	}
	if p.idem != nil {
		seen, err := p.idem.Seen(ctx, e.EventID)
		if err != nil {
			return fmt.Errorf("event publisher: idempotency check: %w", err)
		}
		if seen {
			return nil
		}
	}
	if e.Seq <= 0 {
		e.Seq = p.nextSeq(e.TaskID)
	}
	if err := e.Validate(); err != nil {
		return err
	}
	if err := p.sink.Append(ctx, []*Event{e}); err != nil {
		if p.buffer != nil {
			if berr := p.buffer.Add(e); berr != nil {
				return fmt.Errorf("%w (buffer: %v)", err, berr)
			}
		}
		return err
	}
	if p.idem != nil {
		_ = p.idem.Mark(ctx, e.EventID)
	}
	return nil
}

func (p *Publisher) PublishBatch(ctx context.Context, events []*Event) error {
	for _, e := range events {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) Flush(ctx context.Context) error {
	if p.buffer == nil {
		return nil
	}
	pending, err := p.buffer.Drain()
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if err := p.sink.Append(ctx, pending); err != nil {
		for _, e := range pending {
			_ = p.buffer.Add(e)
		}
		return err
	}
	return nil
}

func (p *Publisher) Pending() int {
	if p.buffer == nil {
		return 0
	}
	return p.buffer.Len()
}
