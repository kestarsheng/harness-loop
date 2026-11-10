package main

import (
	"os"

	"agent-platform/internal/cli/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
