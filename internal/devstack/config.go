package devstack

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The templates under services/*/config.example.yaml are written for docker
// compose (service DNS names, container paths, compose ports). RenderConfig
// localizes them for the host stack, mirroring the sed rewrites in
// scripts/dev-up.sh, but with ports and aliases derived from the contract.

var (
	// host:port pairs such as "redis:6379" or "harnessloop-service:50008".
	hostPortPattern   = regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9-]*):(\d+)\b`)
	pathLinePattern   = regexp.MustCompile(`^(\s*path:\s*).*$`)
	dsnLinePattern    = regexp.MustCompile(`^(\s*dsn:\s*).*$`)
	entryAgentPattern = regexp.MustCompile(`^(\s*entry_agent:\s*).*$`)
	addrLinePattern   = regexp.MustCompile(`^(\s*address:\s*)["']?[:0-9]+["']?\s*$`)
	portLinePattern   = regexp.MustCompile(`^(\s*(?:grpc_port|http_port):\s*)\d+\s*$`)
	keyLinePattern    = regexp.MustCompile(`^(\s*[a-z_]+:\s*)`)
	// llmLinePattern matches keys that must be filled from .harness/dev/env;
	// the section guard keeps MCP server keys untouched.
	llmLinePattern    = regexp.MustCompile(`^(\s+)(provider|api_key|base_url|model|embedding_model):\s*.*$`)
	topSectionPattern = regexp.MustCompile(`^([a-z_]+):`)
)

// RenderConfig writes config_template to config_output with host-local values.
func RenderConfig(root string, contract *Contract, item Item) error {
	if item.ConfigTemplate == "" {
		return nil
	}
	tpl := absJoin(root, item.ConfigTemplate)
	out := absJoin(root, item.ConfigOutput)
	raw, err := os.ReadFile(tpl)
	if err != nil {
		return fmt.Errorf("read config template %s: %w", tpl, err)
	}

	rendered := localizeConfig(string(raw), contract, item, loadDevEnv(root))

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(out, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("write config %s: %w", out, err)
	}
	return nil
}

// loadDevEnv parses the bash-style .harness/dev/env. Missing files are treated
// as empty so templates keep their placeholders and the service surfaces the
// real error instead of inventing credentials.
func loadDevEnv(root string) map[string]string {
	raw, err := os.ReadFile(absJoin(root, ".harness/dev/env"))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key != "" && value != "" {
			out[key] = value
		}
	}
	return out
}

// fillLLM mirrors scripts/dev-up.sh: it injects provider/api_key/base_url/
// model/embedding_model into the llm: block only, so MCP server credentials
// under tools:/services: are left alone.
func fillLLM(text string, env map[string]string) string {
	key := env["HARNESS_LLM_API_KEY"]
	base := env["HARNESS_LLM_BASE_URL"]
	model := env["HARNESS_LLM_MODEL"]
	embeddingModel := env["HARNESS_EMBEDDING_MODEL"]
	if embeddingModel == "" {
		embeddingModel = model
	}
	if key == "" && base == "" && model == "" {
		return text
	}
	provider := "openai"
	if strings.Contains(base, "dashscope") {
		provider = "dashscope"
	}

	lines := strings.Split(text, "\n")
	section := ""
	for i, line := range lines {
		if m := topSectionPattern.FindStringSubmatch(line); m != nil {
			section = m[1]
			continue
		}
		if section != "llm" {
			continue
		}
		m := llmLinePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[2] {
		case "provider":
			if provider != "" {
				lines[i] = m[1] + "provider: " + provider
			}
		case "api_key":
			if key != "" {
				lines[i] = m[1] + "api_key: " + strconv.Quote(key)
			}
		case "base_url":
			if base != "" {
				lines[i] = m[1] + "base_url: " + strconv.Quote(base)
			}
		case "model":
			if model != "" {
				lines[i] = m[1] + "model: " + strconv.Quote(model)
			}
		case "embedding_model":
			if embeddingModel != "" {
				lines[i] = m[1] + "embedding_model: " + strconv.Quote(embeddingModel)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func localizeConfig(text string, contract *Contract, item Item, env map[string]string) string {
	aliases := hostAliases(contract)
	pgDSN := postgresDSN(contract)
	dbName := strings.TrimSuffix(item.Name, "-service") + ".db"

	text = fillLLM(text, env)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		// dev-up.sh pins the harnessloop entry agent to the real registered one;
		// "default" is only a placeholder in the template.
		if m := entryAgentPattern.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + `"main-agent"`
			continue
		}
		// Containers use /app/data; host runs use .harness/dev/<name>.db.
		if m := pathLinePattern.FindStringSubmatch(line); m != nil && strings.Contains(line, ".db") {
			lines[i] = m[1] + filepath.ToSlash(filepath.Join(".harness", "dev", dbName))
			continue
		}
		if pgDSN != "" {
			if m := dsnLinePattern.FindStringSubmatch(line); m != nil {
				lines[i] = m[1] + strconv.Quote(pgDSN)
				continue
			}
		}
		if m := addrLinePattern.FindStringSubmatch(line); m != nil && item.Port > 0 {
			lines[i] = m[1] + strconv.Quote("127.0.0.1:"+strconv.Itoa(item.Port))
			continue
		}
		if m := portLinePattern.FindStringSubmatch(line); m != nil {
			switch keyValue(line) {
			case "grpc_port":
				if item.Port > 0 {
					lines[i] = m[1] + strconv.Itoa(item.Port)
					continue
				}
			case "http_port":
				if item.HTTPPort > 0 {
					lines[i] = m[1] + strconv.Itoa(item.HTTPPort)
					continue
				}
			}
		}
		if alias, port, ok := matchHostPort(line); ok {
			if target, found := aliases[strings.ToLower(alias)]; found {
				lines[i] = strings.Replace(line, alias+":"+strconv.Itoa(port), "127.0.0.1:"+strconv.Itoa(target), 1)
			}
		}
	}
	rendered := strings.Join(lines, "\n")
	// dev-up.sh repoints config/harness_loop/*.yaml at pkg/config/harness_loop/.
	rendered = strings.ReplaceAll(rendered, `"config/harness_loop/`, `"pkg/config/harness_loop/`)
	return rendered
}

// hostAliases maps compose DNS names to the contract item's host port.
func hostAliases(contract *Contract) map[string]int {
	out := map[string]int{}
	if contract == nil {
		return out
	}
	for _, it := range contract.Items {
		out[strings.ToLower(it.Name)] = it.Port
		if it.Kind != KindContainer {
			out[strings.ToLower(it.Name)+"-service"] = it.Port
		}
	}
	// The harnessloop item is named "harnessloop" but its compose service is
	// "harnessloop-service"; the generic suffix above already covers it.
	out["postgres"] = portOf(contract, "postgres")
	out["pg"] = portOf(contract, "postgres")
	return out
}

func portOf(contract *Contract, name string) int {
	if it, ok := contract.Item(name); ok {
		return it.Port
	}
	return 0
}

// postgresDSN reuses the DSN the contract already declares for memory-service.
func postgresDSN(contract *Contract) string {
	if contract == nil {
		return ""
	}
	if mem, ok := contract.Item("memory"); ok {
		if dsn := mem.Env["HARNESS_MEM_PG_DSN"]; dsn != "" {
			return dsn
		}
	}
	if pg, ok := contract.Item("postgres"); ok {
		return fmt.Sprintf("postgres://agent:agent_dev_password@127.0.0.1:%d/agent_platform?sslmode=disable", pg.Port)
	}
	return ""
}

func keyValue(line string) string {
	m := keyLinePattern.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(m[1], ":"))
}

func matchHostPort(line string) (string, int, bool) {
	m := hostPortPattern.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	port, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], port, true
}
