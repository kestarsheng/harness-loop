package repository

import (
	"errors"
	"strings"
	"testing"

	"agent-platform/services/knowledge-service/internal/search"
)

func uploadCode(err error) string {
	var ue *UploadError
	if errors.As(err, &ue) {
		return ue.Code
	}
	return ""
}

func TestDocumentStateUploadValidation(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content []byte
		want    string
	}{
		{"exe rejected", "virus.exe", []byte("x"), "E_UNSUPPORTED_FORMAT"},
		{"empty rejected", "doc.md", nil, "E_EMPTY_DOCUMENT"},
		{"blank rejected", "doc.md", []byte("   \n "), "E_EMPTY_DOCUMENT"},
		{"too large rejected", "doc.pdf", make([]byte, 1024), "E_DOCUMENT_TOO_LARGE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			max := int64(MaxDocumentBytes)
			if c.want == "E_DOCUMENT_TOO_LARGE" {
				max = 10
			}
			err := ValidateUpload(c.file, c.content, max)
			if got := uploadCode(err); got != c.want {
				t.Fatalf("code = %q, want %q (err=%v)", got, c.want, err)
			}
		})
	}

	if err := ValidateUpload("doc.md", []byte("# ok"), 0); err != nil {
		t.Fatalf("valid md rejected: %v", err)
	}
	for _, name := range []string{"a.pdf", "a.docx", "a.txt", "a.html", "a.htm", "a.markdown"} {
		if err := ValidateUpload(name, []byte("content"), 0); err != nil {
			t.Fatalf("%s must be accepted: %v", name, err)
		}
	}
}

func TestDocumentStateTransitions(t *testing.T) {
	legal := [][2]string{
		{StatusPending, StatusProcessing},
		{StatusProcessing, StatusReady},
		{StatusProcessing, StatusFailed},
		{StatusFailed, StatusPending},
		{StatusReady, StatusProcessing},
	}
	for _, pair := range legal {
		if !AllowedTransition(pair[0], pair[1]) {
			t.Fatalf("%s->%s must be legal", pair[0], pair[1])
		}
	}
	illegal := [][2]string{
		{StatusPending, StatusReady},
		{StatusFailed, StatusReady},
		{StatusReady, StatusPending},
		{StatusFailed, StatusProcessing},
	}
	for _, pair := range illegal {
		if AllowedTransition(pair[0], pair[1]) {
			t.Fatalf("%s->%s must be illegal", pair[0], pair[1])
		}
	}
}

func TestDocumentStateVisibility(t *testing.T) {
	if !Searchable(StatusReady) {
		t.Fatal("ready must be searchable")
	}
	for _, s := range []string{StatusPending, StatusProcessing, StatusFailed} {
		if Searchable(s) {
			t.Fatalf("%s must not be searchable", s)
		}
		if !Visible(s) {
			t.Fatalf("%s must stay visible in lists", s)
		}
	}
	if !Retryable(StatusFailed) {
		t.Fatal("failed must be retryable")
	}
	if Retryable(StatusReady) || Retryable(StatusPending) {
		t.Fatal("only failed is retryable")
	}
	if !ValidStatus(StatusFailed) || ValidStatus("unknown") {
		t.Fatal("status validation broken")
	}
}

func TestDocumentStateRetryPolicy(t *testing.T) {
	if got := NextStatusOnFailure(0, 3); got != StatusPending {
		t.Fatalf("first failure -> %s", got)
	}
	if got := NextStatusOnFailure(2, 3); got != StatusPending {
		t.Fatalf("third failure under limit -> %s", got)
	}
	if got := NextStatusOnFailure(3, 3); got != StatusFailed {
		t.Fatalf("exhausted -> %s", got)
	}
}

func TestDocumentStateKBMatches(t *testing.T) {
	if !KBMatches("kb-a", "") {
		t.Fatal("empty requested kb must match anything")
	}
	if !KBMatches("kb-a", "kb-a") {
		t.Fatal("same kb must match")
	}
	if KBMatches("kb-a", "kb-b") {
		t.Fatal("different kb must not match")
	}
	if KBMatches("", "kb-b") {
		t.Fatal("missing document kb must not match a requested kb")
	}
}

func TestDocumentStateBM25RealScores(t *testing.T) {
	idx := search.NewBM25()
	idx.AddDocument("c1", "login flow handler with tests")
	idx.AddDocument("c2", "database migration script")
	results := idx.Search("login tests", 5)
	if len(results) == 0 {
		t.Fatal("bm25 must return hits")
	}
	if results[0].ID != "c1" || results[0].Score <= 0 {
		t.Fatalf("real score expected, got %+v", results[0])
	}
	for _, r := range results {
		if r.Score <= 0 {
			t.Fatalf("score must be positive: %+v", r)
		}
	}

	if !idx.RemoveDocument("c1") {
		t.Fatal("remove existing doc must succeed")
	}
	if idx.RemoveDocument("c1") {
		t.Fatal("second remove must fail")
	}
	after := idx.Search("login tests", 5)
	for _, r := range after {
		if r.ID == "c1" {
			t.Fatalf("removed doc must not be searchable: %+v", after)
		}
	}
	if idx.GetDocumentCount() != 1 {
		t.Fatalf("doc count = %d", idx.GetDocumentCount())
	}
	idx.AddDocument("c1", "login flow handler with tests")
	if got := idx.Search("login tests", 5); len(got) == 0 || got[0].ID != "c1" {
		t.Fatalf("re-added doc must be searchable: %+v", got)
	}
}

func TestDocumentStateBM25IncrementalStats(t *testing.T) {
	idx := search.NewBM25()
	idx.AddDocument("a", strings.Repeat("alpha ", 20))
	idx.AddDocument("b", "alpha beta")
	before := idx.Search("alpha", 5)
	if len(before) != 2 {
		t.Fatalf("both docs should match alpha: %+v", before)
	}
	idx.RemoveDocument("a")
	after := idx.Search("alpha", 5)
	if len(after) != 1 || after[0].ID != "b" {
		t.Fatalf("incremental removal failed: %+v", after)
	}
	if after[0].Score <= 0 {
		t.Fatalf("score must remain real: %+v", after[0])
	}
}
