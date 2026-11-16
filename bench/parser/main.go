// Command parser-bench is the T-P2-T3 evaluation entry point. It hosts two
// independent measurements that share the same annotated fixtures:
//
//	parser     symbol/import/call accuracy for Go, TypeScript and Python
//	knowledge  faithfulness / relevancy / precision on the fixed knowledge set
//
// Run from the repository root, e.g.
//
//	go run ./bench/parser parser -min-f1 0.90
//	go run ./bench/parser knowledge -json bench/parser/out/knowledge.json
package main

import (
	"fmt"
	"os"
)

func main() {
	command := "parser"
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "parser" || args[0] == "knowledge") {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "parser":
		os.Exit(runAccuracy(args))
	case "knowledge":
		os.Exit(runKnowledge(args))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		os.Exit(2)
	}
}
