package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-platform/pkg/harnessloop/policy/sandbox"
)

// WorkspaceRootFromConfig extracts the task workspace root that agent-service
// forwards on every harness call through CallToolRequest.tool_config. Both the
// reserved top-level key and its extra-nested form are accepted, matching
// service.policyContextFromToolConfig.
//
// This is the only legitimate root for file tools: the MCP process cwd is the
// service directory, not the task workspace.
func WorkspaceRootFromConfig(config map[string]interface{}) string {
	section := policyContextSection(config)
	if section == nil {
		return ""
	}
	if root, ok := section["workspace_root"].(string); ok {
		return strings.TrimSpace(root)
	}
	return ""
}

func policyContextSection(config map[string]interface{}) map[string]interface{} {
	if config == nil {
		return nil
	}
	if section, ok := config["__policy_context__"].(map[string]interface{}); ok {
		return section
	}
	if extra, ok := config["extra"].(map[string]interface{}); ok {
		if section, ok := extra["__policy_context__"].(map[string]interface{}); ok {
			return section
		}
	}
	return nil
}

// resolveWorkspacePath maps a tool path argument onto an absolute path inside
// root. Resolution is delegated to policy/sandbox so the file the tool writes
// and the path output_contract later stats are obtained the same way: relative
// to the workspace root, never the process cwd. Absolute paths, `..` escapes,
// UNC paths and URL-style paths are refused there.
func resolveWorkspacePath(root, rel string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("%w: workspace root is not configured for this call", sandbox.ErrUnsafePath)
	}
	return sandbox.Resolve(root, rel)
}

// writeWorkspaceFile writes content atomically under dir and then, before
// returning, asserts the file is really readable from the workspace. A caller
// never sees success for an artifact that is not on disk.
func writeWorkspaceFile(abs string, content []byte) (int64, string, error) {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".artifact-*")
	if err != nil {
		return 0, "", fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		cleanup()
		return 0, "", fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return 0, "", fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return 0, "", fmt.Errorf("chmod %s: %w", abs, err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		cleanup()
		return 0, "", fmt.Errorf("rename into %s: %w", abs, err)
	}

	// Assert after the write: size and hash of what the artifact checker will
	// see, computed from the file on disk rather than from the input buffer.
	stored, err := os.ReadFile(abs)
	if err != nil {
		return 0, "", fmt.Errorf("verify %s: %w", abs, err)
	}
	if len(stored) != len(content) {
		return 0, "", fmt.Errorf("verify %s: wrote %d bytes, read %d", abs, len(content), len(stored))
	}
	sum := sha256.Sum256(stored)
	return int64(len(stored)), "sha256:" + hex.EncodeToString(sum[:]), nil
}

// relPathForResult renders the stored path the way output_contract declares
// it, so callers can compare the tool result with the contract string.
func relPathForResult(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}
