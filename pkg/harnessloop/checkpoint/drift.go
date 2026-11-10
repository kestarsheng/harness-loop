package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrDrift = errors.New("checkpoint drift detected")
var ErrNoUsableCheckpoint = errors.New("no usable checkpoint")

type DriftAction string

const (
	DriftBlock   DriftAction = "block"
	DriftAccept  DriftAction = "accept"
	DriftRebuild DriftAction = "rebuild"
)

type DriftField struct {
	Field    string
	Expected string
	Actual   string
}

type DriftReport struct {
	Drifted bool
	Fields  []DriftField
	Action  DriftAction
}

func (d *DriftReport) Add(field, expected, actual string) {
	d.Drifted = true
	d.Fields = append(d.Fields, DriftField{Field: field, Expected: expected, Actual: actual})
}

type TaskSnapshot struct {
	Stage       string
	AttemptNo   int
	Consumed    Consumed
	EventMaxSeq int64
}

type DriftPolicy struct {
	OnFMSDrift       DriftAction
	OnArtifactDrift  DriftAction
	OnWatermarkAhead DriftAction
	OnConsumedDrift  DriftAction
}

func DefaultDriftPolicy() DriftPolicy {
	return DriftPolicy{
		OnFMSDrift:       DriftBlock,
		OnArtifactDrift:  DriftBlock,
		OnWatermarkAhead: DriftRebuild,
		OnConsumedDrift:  DriftAccept,
	}
}

type HashFunc func(path string) (string, error)

func DefaultHashFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func DetectDrift(cp *Checkpoint, task TaskSnapshot, workspace string, policy DriftPolicy, hashFile HashFunc) *DriftReport {
	report := &DriftReport{Action: DriftBlock}

	if cp.FSM.CurrentStage != task.Stage {
		report.Add("fsm.current_stage", cp.FSM.CurrentStage, task.Stage)
	}
	if cp.AttemptNo != task.AttemptNo {
		report.Add("attempt_no", fmt.Sprintf("%d", cp.AttemptNo), fmt.Sprintf("%d", task.AttemptNo))
	}
	if report.Drifted {
		report.Action = policy.OnFMSDrift
	}

	if workspace != "" && len(cp.Artifacts) > 0 {
		if hashFile == nil {
			hashFile = DefaultHashFile
		}
		for _, a := range cp.Artifacts {
			p, err := safeJoin(workspace, a.Path)
			if err != nil {
				report.Add("artifact."+a.Path, a.Hash, "path escapes workspace")
				continue
			}
			got, err := hashFile(p)
			if err != nil {
				report.Add("artifact."+a.Path, a.Hash, "missing")
				continue
			}
			if got != a.Hash {
				report.Add("artifact."+a.Path, a.Hash, got)
			}
		}
		if len(report.Fields) > 0 && policy.OnArtifactDrift != "" {
			report.Action = policy.OnArtifactDrift
		}
	}

	if cp.Consumed.Tokens != task.Consumed.Tokens || cp.Consumed.Cost != task.Consumed.Cost {
		report.Add("consumed",
			fmt.Sprintf("%d/%.4f", cp.Consumed.Tokens, cp.Consumed.Cost),
			fmt.Sprintf("%d/%.4f", task.Consumed.Tokens, task.Consumed.Cost))
		if report.Action == DriftBlock && policy.OnConsumedDrift != "" {
			report.Action = policy.OnConsumedDrift
		}
	}

	if cp.EventSeqWatermark > task.EventMaxSeq {
		report.Add("event_seq_watermark",
			fmt.Sprintf("%d", cp.EventSeqWatermark),
			fmt.Sprintf("%d", task.EventMaxSeq))
		report.Action = policy.OnWatermarkAhead
	}

	if !report.Drifted {
		report.Action = ""
	}
	return report
}

var errPathEscape = errors.New("path escapes workspace")

func safeJoin(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", errPathEscape
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Clean(filepath.Join(rootAbs, filepath.FromSlash(rel)))
	if target != rootAbs && !strings.HasPrefix(target, rootAbs+string(filepath.Separator)) {
		return "", errPathEscape
	}
	return target, nil
}
