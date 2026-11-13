package memoryfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keyOrder(doc *Document) string {
	return strings.Join(doc.Order, ",")
}

func TestIntegrationEvictionBoundaryAt31(t *testing.T) {
	s := newProjectStore()
	for i := 1; i <= 30; i++ {
		if err := s.Insert(topicFor(fmt.Sprintf("t%02d", i), CategoryProjectFact, "body", false)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if got := len(s.Document().Topics); got != 30 {
		t.Fatalf("topics at limit = %d, want 30", got)
	}
	if evictions := s.DrainEvictions(); len(evictions) != 0 {
		t.Fatalf("no eviction before limit, got %+v", evictions)
	}
	if got := keyOrder(s.Document()); got != "t30,t29,t28,t27,t26,t25,t24,t23,t22,t21,t20,t19,t18,t17,t16,t15,t14,t13,t12,t11,t10,t09,t08,t07,t06,t05,t04,t03,t02,t01" {
		t.Fatalf("order at limit = %s", got)
	}

	if err := s.Insert(topicFor("t31", CategoryProjectFact, "body", false)); err != nil {
		t.Fatalf("insert 31st: %v", err)
	}
	doc := s.Document()
	if len(doc.Topics) != 30 {
		t.Fatalf("topics after 31st = %d, want 30", len(doc.Topics))
	}
	if _, ok := doc.TopicByKey("t01"); ok {
		t.Fatal("31st insert must evict oldest t01")
	}
	if doc.Order[0] != "t31" || doc.Order[len(doc.Order)-1] != "t02" {
		t.Fatalf("order after eviction head=%s tail=%s", doc.Order[0], doc.Order[len(doc.Order)-1])
	}
	if len(doc.Order) != len(doc.Topics) {
		t.Fatalf("order/topics length mismatch: %d/%d", len(doc.Order), len(doc.Topics))
	}
	evictions := s.DrainEvictions()
	if len(evictions) != 1 || evictions[0].Key != "t01" || evictions[0].Reason != "lru" {
		t.Fatalf("evictions = %+v", evictions)
	}

	if err := s.Insert(topicFor("t32", CategoryProjectFact, "body", false)); err != nil {
		t.Fatalf("insert 32nd: %v", err)
	}
	evictions = s.DrainEvictions()
	if len(evictions) != 1 || evictions[0].Key != "t02" {
		t.Fatalf("second eviction = %+v", evictions)
	}
	if len(s.Document().Topics) != 30 {
		t.Fatalf("topics after 32nd = %d", len(s.Document().Topics))
	}
}

func TestIntegrationEvictionAfterReadPromotion(t *testing.T) {
	s := newProjectStore()
	for i := 1; i <= 30; i++ {
		if err := s.Insert(topicFor(fmt.Sprintf("t%02d", i), CategoryProjectFact, "body", false)); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.Get("t01"); !ok {
		t.Fatal("t01 must exist")
	}
	if s.Document().Order[0] != "t01" {
		t.Fatalf("read promotion head = %s", s.Document().Order[0])
	}
	if err := s.Insert(topicFor("t31", CategoryProjectFact, "body", false)); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Document().TopicByKey("t01"); !ok {
		t.Fatal("promoted t01 must survive the next eviction")
	}
	if _, ok := s.Document().TopicByKey("t02"); ok {
		t.Fatal("t02 is the least recently used and must be evicted")
	}
	evictions := s.DrainEvictions()
	if len(evictions) != 1 || evictions[0].Key != "t02" {
		t.Fatalf("evictions = %+v", evictions)
	}
}

func TestIntegrationPinnedTailTopicsAreExempt(t *testing.T) {
	s := newProjectStore()
	for _, key := range []string{"p1", "p2", "p3"} {
		if err := s.Insert(topicFor(key, CategoryLongTermConstraint, "- "+key, true)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 27; i++ {
		if err := s.Insert(topicFor(fmt.Sprintf("t%02d", i), CategoryProjectFact, "body", false)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Document().Topics) != 30 {
		t.Fatalf("topics = %d", len(s.Document().Topics))
	}
	if got := keyOrder(s.Document()); !strings.HasSuffix(got, "p3,p2,p1") {
		t.Fatalf("pinned topics must sit at the LRU tail, got %s", got)
	}

	if err := s.Insert(topicFor("t28", CategoryProjectFact, "body", false)); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(topicFor("t29", CategoryProjectFact, "body", false)); err != nil {
		t.Fatal(err)
	}
	doc := s.Document()
	for _, key := range []string{"p1", "p2", "p3"} {
		if _, ok := doc.TopicByKey(key); !ok {
			t.Fatalf("pinned %s must survive eviction", key)
		}
	}
	for _, key := range []string{"t01", "t02"} {
		if _, ok := doc.TopicByKey(key); ok {
			t.Fatalf("unpinned %s must be evicted before any pinned topic", key)
		}
	}
	evictions := s.DrainEvictions()
	if len(evictions) != 2 {
		t.Fatalf("evictions = %+v", evictions)
	}
	for _, ev := range evictions {
		if strings.HasPrefix(ev.Key, "p") {
			t.Fatalf("pinned topic evicted: %+v", ev)
		}
	}
	if len(doc.Topics) != 30 {
		t.Fatalf("topics = %d", len(doc.Topics))
	}
}

const validTopicBlockA = `## a
<!-- meta: category=task_state; pinned=false; updated_at=2026-11-11T00:00:00Z; content_hash=sha256:a -->
### 主题 A
内容 A
`

const validTopicBlockB = `## b
<!-- meta: category=project_fact; pinned=false; updated_at=2026-11-11T00:00:00Z; content_hash=sha256:b -->
### 主题 B
内容 B
`

func TestIntegrationRecoverInvalidFrontmatterKeepsValidTopics(t *testing.T) {
	cases := []struct {
		name        string
		frontmatter string
		order       string
		body        string
		wantKeys    []string
	}{
		{
			name:        "unknown frontmatter key",
			frontmatter: "schema_version: 1\nscope: project\nupdated_at: 2026-11-11T00:00:00Z\norder_version: 7\nbogus: yes\n",
			order:       `["a","b"]`,
			body:        validTopicBlockA + "\n" + validTopicBlockB,
			wantKeys:    []string{"a", "b"},
		},
		{
			name:        "invalid scope",
			frontmatter: "schema_version: 1\nscope: team\nupdated_at: 2026-11-11T00:00:00Z\norder_version: 2\n",
			order:       `["a","b"]`,
			body:        validTopicBlockA + "\n" + validTopicBlockB,
			wantKeys:    []string{"a", "b"},
		},
		{
			name:        "order is not json",
			frontmatter: "schema_version: 1\nscope: project\nupdated_at: 2026-11-11T00:00:00Z\norder_version: 2\n",
			order:       `not-json`,
			body:        validTopicBlockA + "\n" + validTopicBlockB,
			wantKeys:    []string{"a", "b"},
		},
		{
			name:        "duplicate topic keys are deduplicated",
			frontmatter: "schema_version: 1\nscope: project\nupdated_at: 2026-11-11T00:00:00Z\norder_version: 2\nbogus: yes\n",
			order:       `["a"]`,
			body:        validTopicBlockA + "\n" + validTopicBlockA,
			wantKeys:    []string{"a"},
		},
		{
			name:        "invalid meta drops only that block",
			frontmatter: "schema_version: 1\nscope: project\nupdated_at: 2026-11-11T00:00:00Z\norder_version: 2\nbogus: yes\n",
			order:       `["a","bad"]`,
			body:        validTopicBlockA + "\n## bad\n<!-- meta: category=nope; pinned=false; updated_at=2026-11-11T00:00:00Z; content_hash=sha256:x -->\n### 坏块\n内容\n",
			wantKeys:    []string{"a"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "project.md")
			backupDir := filepath.Join(dir, "backup")
			raw := "---\n" + tc.frontmatter + "---\n\n<!-- order: " + tc.order + " -->\n\n" + tc.body
			if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("test fixture must be invalid markdown before recovery")
			}

			res, err := RecoverFile(path, backupDir, fixedClock)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if !res.Corrupted || res.Retained != len(tc.wantKeys) {
				t.Fatalf("result = %+v", res)
			}
			if res.Document == nil {
				t.Fatal("recovered document is nil")
			}
			gotKeys := strings.Join(res.Document.Order, ",")
			if gotKeys != strings.Join(tc.wantKeys, ",") {
				t.Fatalf("recovered order = %s, want %v", gotKeys, tc.wantKeys)
			}
			backupRaw, err := os.ReadFile(res.BackupPath)
			if err != nil {
				t.Fatalf("read backup: %v", err)
			}
			if string(backupRaw) != raw {
				t.Fatal("backup must preserve the original bytes")
			}

			repaired, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(repaired)
			if err != nil {
				t.Fatalf("repaired file must parse: %v", err)
			}
			if strings.Join(parsed.Order, ",") != strings.Join(tc.wantKeys, ",") {
				t.Fatalf("repaired order = %s", strings.Join(parsed.Order, ","))
			}
			second, err := RecoverFile(path, backupDir, fixedClock)
			if err != nil {
				t.Fatalf("second recover: %v", err)
			}
			if second.Corrupted {
				t.Fatal("repaired file must be healthy on the second pass")
			}
		})
	}
}

func replayEventOrder(events []ChangeEvent) []string {
	var order []string
	remove := func(key string) {
		for i, k := range order {
			if k == key {
				order = append(order[:i], order[i+1:]...)
				return
			}
		}
	}
	insertAt := func(key string, rank int) {
		remove(key)
		if rank < 0 || rank > len(order) {
			rank = len(order)
		}
		order = append(order, "")
		copy(order[rank+1:], order[rank:])
		order[rank] = key
	}
	for _, event := range events {
		switch event.Type {
		case EventWritten:
			for _, change := range event.Topics {
				switch change.Action {
				case string(ActionCreated), string(ActionUpdated):
					insertAt(change.Key, change.OrderRank)
				case string(ActionDeleted):
					remove(change.Key)
				}
			}
		case EventEvicted:
			for _, change := range event.Topics {
				remove(change.Key)
			}
		case EventPromoted:
			for _, change := range event.Topics {
				insertAt(change.Key, change.OrderRank)
			}
		}
	}
	return order
}

func TestIntegrationWriteEventsReplayFinalState(t *testing.T) {
	writer, store, path, sink := newWriterFixture(t)
	for _, key := range []string{"a", "b", "c"} {
		if err := store.Insert(topicFor(key, CategoryTaskState, "v1", false)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(topicFor("b", CategoryTaskState, "v2", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	if !store.Remove("c") {
		t.Fatal("remove c failed")
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 28; i++ {
		if err := store.Insert(topicFor(fmt.Sprintf("x%02d", i), CategoryProjectFact, "body", false)); err != nil {
			t.Fatal(err)
		}
	}
	evictions := store.DrainEvictions()
	writer.EmitEvictions(evictions)
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(topicFor("y01", CategoryProjectFact, "body", false)); err != nil {
		t.Fatal(err)
	}
	evictions = store.DrainEvictions()
	writer.EmitEvictions(evictions)
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}

	final := store.Document()
	if len(final.Topics) != 30 {
		t.Fatalf("final topics = %d", len(final.Topics))
	}
	replayed := replayEventOrder(sink.events)
	if strings.Join(replayed, ",") != keyOrder(final) {
		t.Fatalf("replayed order = %s\nfinal order    = %s", strings.Join(replayed, ","), keyOrder(final))
	}
	parsed, err := Parse(mustRead(t, path))
	if err != nil {
		t.Fatalf("final file must parse: %v", err)
	}
	if keyOrder(parsed) != keyOrder(final) {
		t.Fatalf("file order = %s\ndoc order  = %s", keyOrder(parsed), keyOrder(final))
	}
	lastWritten := sink.events[len(sink.events)-1]
	if lastWritten.Type != EventWritten {
		t.Fatalf("last event = %s", lastWritten.Type)
	}
	if lastWritten.OrderVersion != final.OrderVersion {
		t.Fatalf("event order_version = %d, final = %d", lastWritten.OrderVersion, final.OrderVersion)
	}
}

func TestIntegrationPromotionEventReplayRestoresOrder(t *testing.T) {
	writer, store, _, sink := newWriterFixture(t)
	for _, key := range []string{"a", "b", "c"} {
		if err := store.Insert(topicFor(key, CategoryUserPreference, "pref-"+key, false)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("a"); !ok {
		t.Fatal("a must exist")
	}
	writer.EmitPromotion("a", 0)
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	final := store.Document()
	replayed := replayEventOrder(sink.events)
	if strings.Join(replayed, ",") != keyOrder(final) {
		t.Fatalf("replayed order = %s, final = %s", strings.Join(replayed, ","), keyOrder(final))
	}
	promotions := 0
	for _, event := range sink.events {
		if event.Type == EventPromoted {
			promotions++
		}
	}
	if promotions != 1 {
		t.Fatalf("promotion events = %d", promotions)
	}

}
