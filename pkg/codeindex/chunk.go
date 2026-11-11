package codeindex

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"agent-platform/pkg/llm"
	"agent-platform/pkg/memoryfile"
)

type ChunkType string

const (
	ChunkFunction     ChunkType = "function"
	ChunkMethod       ChunkType = "method"
	ChunkHeader       ChunkType = "header"
	ChunkFunctionPart ChunkType = "function_part"
	ChunkFile         ChunkType = "file"
)

const (
	EdgeCall   = "call"
	EdgeImport = "import"
)

type Chunk struct {
	FilePath     string
	StartLine    int
	EndLine      int
	Symbol       string
	ParentSymbol string
	ChunkType    ChunkType
	ContentHash  string
	Text         string
	ParserID     string
	Degraded     bool
}

type Edge struct {
	Source     string
	Target     string
	EdgeType   string
	SourceFile string
	TargetFile string
}

type ParseResult struct {
	Chunks   []Chunk
	Edges    []Edge
	Symbols  []string
	Imports  []string
	Calls    []string
	ParserID string
	Degraded bool
}

func ComputeContentHash(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func IdentifierTokens(identifier string) []string {
	return memoryfile.Keywords(identifier)
}

func NewChunk(filePath string, startLine, endLine int, symbol string, chunkType ChunkType, text, parserID string) Chunk {
	return Chunk{
		FilePath:    filePath,
		StartLine:   startLine,
		EndLine:     endLine,
		Symbol:      symbol,
		ChunkType:   chunkType,
		ContentHash: ComputeContentHash(text),
		Text:        text,
		ParserID:    parserID,
	}
}

func SplitLongFunction(chunk Chunk, maxTokens, windowTokens, overlapTokens int) []Chunk {
	if chunk.ChunkType != ChunkFunction && chunk.ChunkType != ChunkMethod {
		return []Chunk{chunk}
	}
	if maxTokens <= 0 {
		maxTokens = 1200
	}
	if windowTokens <= 0 {
		windowTokens = 800
	}
	if overlapTokens < 0 {
		overlapTokens = 100
	}
	if llm.EstimateTokens(chunk.Text) <= maxTokens {
		return []Chunk{chunk}
	}

	lines := strings.Split(chunk.Text, "\n")
	lineTokens := make([]int, len(lines))
	for i, line := range lines {
		lineTokens[i] = llm.EstimateTokens(line)
	}

	var parts []Chunk
	for start := 0; start < len(lines); {
		used := 0
		end := start
		for end < len(lines) {
			if used > 0 && used+lineTokens[end] > windowTokens {
				break
			}
			used += lineTokens[end]
			end++
		}
		if end == start {
			end = start + 1
		}
		text := strings.Join(lines[start:end], "\n")
		part := Chunk{
			FilePath:     chunk.FilePath,
			StartLine:    chunk.StartLine + start,
			EndLine:      chunk.StartLine + end - 1,
			Symbol:       fmt.Sprintf("%s#part%02d", chunk.Symbol, len(parts)+1),
			ParentSymbol: chunk.Symbol,
			ChunkType:    ChunkFunctionPart,
			ContentHash:  ComputeContentHash(text),
			Text:         text,
			ParserID:     chunk.ParserID,
			Degraded:     chunk.Degraded,
		}
		parts = append(parts, part)
		if end >= len(lines) {
			break
		}
		next := end - overlapTokens/2
		if next <= start {
			next = end
		}
		start = next
	}
	return parts
}
