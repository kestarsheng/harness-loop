package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
	agentpb "agent-platform/pkg/pb/agent"
)

// SubtaskExecutorConfig carries the agent-service parameters a subtask run
// needs. The parent task, tenant and workspace come from the stage context.
type SubtaskExecutorConfig struct {
	EntryAgent string
	UserID     string
	Model      string
	Timeout    time.Duration
}

// AgentSubtaskExecutor runs a scheduled subtask through agent-service's
// ExecuteStream, reusing the same streaming contract as the stage runner. It
// never invents a second execution path: every subtask is one agent run.
type AgentSubtaskExecutor struct {
	agents AgentStreamer
	cfg    SubtaskExecutorConfig
}

func NewAgentSubtaskExecutor(agents AgentStreamer, cfg SubtaskExecutorConfig) *AgentSubtaskExecutor {
	if cfg.EntryAgent == "" {
		cfg.EntryAgent = "default"
	}
	if cfg.UserID == "" {
		cfg.UserID = "harness-loop"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	return &AgentSubtaskExecutor{agents: agents, cfg: cfg}
}

func (e *AgentSubtaskExecutor) Execute(ctx context.Context, ec OrchestratorStageContext, st *subtask.Subtask) (*SubtaskResult, error) {
	if e == nil || e.agents == nil {
		return nil, fmt.Errorf("subtask executor: agent-service streamer is not configured")
	}
	if st == nil {
		return nil, fmt.Errorf("subtask executor: subtask is nil")
	}

	runCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	req := &agentpb.ExecuteStreamRequest{
		SessionId:       subtaskSessionID(ec.TaskID, st.SubtaskID),
		TenantId:        firstNonEmpty(st.TenantID, ec.TenantID),
		UserId:          e.cfg.UserID,
		Message:         subtaskPrompt(st),
		EntryAgent:      e.cfg.EntryAgent,
		Goal:            subtaskGoal(st),
		SuccessCriteria: strings.Join(st.AcceptanceCriteria, "\n"),
		ContextVars: map[string]string{
			VarStage:             ec.Stage,
			VarStageMode:         "false",
			VarTaskID:            ec.TaskID,
			VarProjectID:         ec.ProjectID,
			VarTenantID:          firstNonEmpty(st.TenantID, ec.TenantID),
			VarWorkspaceRoot:     ec.WorkspaceRoot,
			VarAgentID:           e.cfg.EntryAgent,
			"__subtask_id__":     st.SubtaskID,
			"__parent_task_id__": st.ParentTaskID,
			"__subtask_role__":   st.Role,
			"__dag_id__":         st.DagID,
			"__dag_node_id__":    st.DagNodeID,
		},
	}
	if e.cfg.Model != "" {
		req.ContextVars[VarStageModel] = e.cfg.Model
	}

	result := &SubtaskResult{AcceptanceID: fmt.Sprintf("acc-%s", st.SubtaskID)}
	var content strings.Builder
	var streamErr string
	maxTokens := int64(0)

	err := e.agents.ExecuteStream(runCtx, req, func(chunk *agentpb.ExecuteStreamChunk) error {
		if chunk == nil {
			return nil
		}
		switch chunk.Type {
		case "response", "token", "message":
			if chunk.Content != "" {
				content.WriteString(chunk.Content)
			}
		case "error":
			if chunk.Content != "" {
				streamErr = chunk.Content
			}
		}
		if chunk.Step != nil && int64(chunk.Step.TokensUsed) > maxTokens {
			maxTokens = int64(chunk.Step.TokensUsed)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("subtask %s: execute stream: %w", st.SubtaskID, err)
	}
	if streamErr != "" {
		return nil, fmt.Errorf("subtask %s: agent stream error: %s", st.SubtaskID, streamErr)
	}

	artifacts, artErr := collectSubtaskArtifacts(ec.WorkspaceRoot, st)
	if artErr != nil {
		return nil, fmt.Errorf("subtask %s: %w", st.SubtaskID, artErr)
	}
	result.Artifacts = artifacts
	result.Conclusion = truncateConclusion(content.String())
	st.ConsumedTokens = maxTokens
	if result.Conclusion != "" {
		if st.OutputContract == nil {
			st.OutputContract = make(map[string]any)
		}
		st.OutputContract["conclusion"] = result.Conclusion
	}
	return result, nil
}

func subtaskSessionID(taskID, subtaskID string) string {
	if taskID == "" {
		return subtaskID
	}
	return taskID + "-" + subtaskID
}

func subtaskPrompt(st *subtask.Subtask) string {
	var b strings.Builder
	b.WriteString("你正在执行父任务的一个子任务，请只完成本子任务并给出可核验的结论。\n")
	fmt.Fprintf(&b, "角色: %s\n子任务ID: %s\n父任务ID: %s\n", st.Role, st.SubtaskID, st.ParentTaskID)
	if len(st.InputContract) > 0 {
		fmt.Fprintf(&b, "输入契约: %s\n", marshalContract(st.InputContract))
	}
	if len(st.OutputContract) > 0 {
		fmt.Fprintf(&b, "输出契约: %s\n", marshalContract(st.OutputContract))
	}
	if len(st.AcceptanceCriteria) > 0 {
		b.WriteString("验收标准:\n")
		for _, criterion := range st.AcceptanceCriteria {
			fmt.Fprintf(&b, "- %s\n", criterion)
		}
	}
	b.WriteString("完成后请用简明中文总结本次产出与结论。")
	return b.String()
}

func subtaskGoal(st *subtask.Subtask) string {
	for _, key := range []string{"goal", "objective", "description"} {
		if v, ok := st.InputContract[key]; ok {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	if len(st.AcceptanceCriteria) > 0 {
		return strings.Join(st.AcceptanceCriteria, "；")
	}
	return fmt.Sprintf("完成子任务 %s（角色 %s）", st.SubtaskID, st.Role)
}

func marshalContract(contract map[string]any) string {
	raw, err := json.Marshal(contract)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

const maxConclusionBytes = 4096

func truncateConclusion(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxConclusionBytes {
		return s
	}
	return s[:maxConclusionBytes] + "…"
}

// collectSubtaskArtifacts resolves the output contract paths against the
// workspace. Explicit missing files fail the subtask; globs with no matches
// simply contribute no artifact. Paths stay relative so the aggregator's
// sandbox check can join them with the workspace root.
func collectSubtaskArtifacts(root string, st *subtask.Subtask) ([]subtask.ArtifactRef, error) {
	declared := contractPaths(st.OutputContract)
	if len(declared) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(declared))
	refs := make([]subtask.ArtifactRef, 0, len(declared))
	for _, pattern := range declared {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, `\`) {
			return nil, fmt.Errorf("output contract path %q must be relative to the workspace", pattern)
		}
		hasGlob := strings.ContainsAny(pattern, "*?[")
		matches := []string{pattern}
		if hasGlob {
			base := root
			if base == "" {
				base = "."
			}
			found, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(pattern)))
			if err != nil {
				return nil, fmt.Errorf("expand output contract %q: %w", pattern, err)
			}
			matches = make([]string, 0, len(found))
			for _, full := range found {
				rel, relErr := filepath.Rel(base, full)
				if relErr != nil {
					continue
				}
				matches = append(matches, filepath.ToSlash(rel))
			}
		}
		if len(matches) == 0 && !hasGlob {
			matches = []string{pattern}
		}
		for _, rel := range matches {
			if _, ok := seen[rel]; ok {
				continue
			}
			seen[rel] = struct{}{}
			ref, err := statArtifact(root, rel)
			if err != nil {
				return nil, err
			}
			if ref == nil {
				if hasGlob {
					continue
				}
				return nil, fmt.Errorf("output artifact %q is missing", rel)
			}
			refs = append(refs, *ref)
		}
	}
	return refs, nil
}

func statArtifact(root, rel string) (*subtask.ArtifactRef, error) {
	full := rel
	if root != "" {
		full = filepath.Join(root, filepath.FromSlash(rel))
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat artifact %q: %w", rel, err)
	}
	if info.IsDir() {
		return nil, nil
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("read artifact %q: %w", rel, err)
	}
	sum := sha256.Sum256(raw)
	return &subtask.ArtifactRef{
		Path: filepath.ToSlash(rel),
		Hash: "sha256:" + hex.EncodeToString(sum[:]),
		Size: info.Size(),
	}, nil
}

func contractPaths(contract map[string]any) []string {
	if contract == nil {
		return nil
	}
	var out []string
	for _, key := range []string{"paths", "artifacts", "files"} {
		v, ok := contract[key]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			if val != "" {
				out = append(out, val)
			}
		case []string:
			out = append(out, val...)
		case []any:
			for _, item := range val {
				switch typed := item.(type) {
				case string:
					out = append(out, typed)
				case map[string]any:
					if p, ok := typed["path"].(string); ok && p != "" {
						out = append(out, p)
					}
				}
			}
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
