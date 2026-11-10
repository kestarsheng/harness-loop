package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type RestoreOptions struct {
	Workspace string
	Policy    DriftPolicy
	HashFile  HashFunc
}

type RestoreResult struct {
	Checkpoint   *Checkpoint
	Drift        *DriftReport
	Duplicate    bool
	Rebuild      bool
	FellBackFrom string
	Warnings     []string
}

type restorerKey struct {
	taskID       string
	checkpointID string
}

type Restorer struct {
	mu    sync.Mutex
	store Store
	done  map[restorerKey]bool
}

func NewRestorer(store Store) *Restorer {
	return &Restorer{store: store, done: make(map[restorerKey]bool)}
}

func (r *Restorer) Restore(ctx context.Context, taskID, checkpointID string, task TaskSnapshot, opts RestoreOptions) (*RestoreResult, error) {
	if r.store == nil {
		return nil, errors.New("checkpoint: store is required")
	}
	if opts.Policy == (DriftPolicy{}) {
		opts.Policy = DefaultDriftPolicy()
	}

	key := restorerKey{taskID: taskID, checkpointID: checkpointID}
	r.mu.Lock()
	duplicate := r.done[key]
	r.mu.Unlock()

	cp, fellBackFrom, err := r.loadUsable(ctx, taskID, checkpointID)
	if err != nil {
		return nil, err
	}

	res := &RestoreResult{Checkpoint: cp, Duplicate: duplicate, FellBackFrom: fellBackFrom}
	if fellBackFrom != "" {
		res.Warnings = append(res.Warnings, "checkpoint.corrupted:"+fellBackFrom)
	}

	report := DetectDrift(cp, task, opts.Workspace, opts.Policy, opts.HashFile)
	res.Drift = report
	switch report.Action {
	case DriftBlock:
		return res, fmt.Errorf("%w: %d field(s) drifted", ErrDrift, len(report.Fields))
	case DriftRebuild:
		res.Rebuild = true
	case DriftAccept:
	}

	r.mu.Lock()
	r.done[key] = true
	r.mu.Unlock()
	return res, nil
}

func (r *Restorer) loadUsable(ctx context.Context, taskID, checkpointID string) (*Checkpoint, string, error) {
	if checkpointID != "" {
		cp, err := r.store.Load(ctx, checkpointID)
		if err == nil && cp.Validate() == nil {
			return cp, "", nil
		}
		if err == nil {
			checkpointID = ""
		} else if !errors.Is(err, ErrNotFound) {
			return nil, "", err
		} else {
			checkpointID = ""
		}
	}

	list, err := r.store.List(ctx, taskID, 0)
	if err != nil {
		return nil, "", err
	}
	corrupted := ""
	for _, cp := range list {
		if cp.Validate() == nil {
			return cp, corrupted, nil
		}
		if corrupted == "" {
			corrupted = cp.CheckpointID
		}
	}
	return nil, corrupted, ErrNoUsableCheckpoint
}

func (r *Restorer) RestoreLatest(ctx context.Context, taskID string, task TaskSnapshot, opts RestoreOptions) (*RestoreResult, error) {
	return r.Restore(ctx, taskID, "", task, opts)
}
