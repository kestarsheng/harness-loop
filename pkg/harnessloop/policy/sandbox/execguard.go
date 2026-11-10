package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

var ErrExecDenied = errors.New("E_EXEC_DENIED")

type ExecAllowlist struct {
	Commands []string `yaml:"commands"`
}

func DefaultExecAllowlist() ExecAllowlist {
	return ExecAllowlist{Commands: []string{"go", "gofmt", "golangci-lint", "staticcheck"}}
}

func LoadExecAllowlist(path string) (ExecAllowlist, error) {
	a := DefaultExecAllowlist()
	if path == "" {
		return a, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	var loaded ExecAllowlist
	if err := yaml.Unmarshal(raw, &loaded); err != nil {
		return a, err
	}
	if len(loaded.Commands) > 0 {
		a.Commands = loaded.Commands
	}
	return a, nil
}

func (a ExecAllowlist) Allows(cmd string) bool {
	for _, c := range a.Commands {
		if c == cmd {
			return true
		}
	}
	return false
}

func (a ExecAllowlist) Check(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: empty command", ErrExecDenied)
	}
	cmd := args[0]
	if strings.ContainsAny(cmd, `/\`) || filepath.IsAbs(cmd) {
		return fmt.Errorf("%w: command %q must be a bare executable name", ErrExecDenied, cmd)
	}
	if !a.Allows(cmd) {
		return fmt.Errorf("%w: command %q is not in the allowlist", ErrExecDenied, cmd)
	}
	if cmd == "sh" || cmd == "bash" || cmd == "cmd" || cmd == "powershell" || cmd == "pwsh" {
		return fmt.Errorf("%w: shell invocation is not allowed", ErrExecDenied)
	}
	for _, arg := range args[1:] {
		if strings.ContainsAny(arg, ";|&><`$\n\r") {
			return fmt.Errorf("%w: argument %q contains shell metacharacters", ErrExecDenied, arg)
		}
		if filepath.IsAbs(arg) || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, `\`) {
			return fmt.Errorf("%w: absolute path argument %q is not allowed", ErrExecDenied, arg)
		}
		if strings.HasPrefix(arg, "..") {
			return fmt.Errorf("%w: parent-relative argument %q is not allowed", ErrExecDenied, arg)
		}
	}
	if len(args) > 1 {
		switch args[1] {
		case "install", "get":
			return fmt.Errorf("%w: package installation is not allowed (go %s)", ErrExecDenied, args[1])
		case "mod":
			if len(args) > 2 && (args[2] == "download" || args[2] == "tidy") {
				return fmt.Errorf("%w: dependency mutation is not allowed (go mod %s)", ErrExecDenied, args[2])
			}
		}
	}
	return nil
}
