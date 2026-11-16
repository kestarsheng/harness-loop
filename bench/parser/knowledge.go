// Knowledge evaluation for T-P2-T3: faithfulness / relevancy / context precision
// on a fixed, human annotated dataset.
//
// The harness never calls a real LLM: claims, their supporting documents and the
// key terms a correct answer must mention are annotated by hand, and the answer
// itself is produced by a deterministic extractive baseline. What varies between
// the two runs is the retrieval configuration, so a metric drop points at the
// retrieval stage rather than at model sampling noise. This is stated again in
// bench/p2/knowledge-eval.md.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type knowledgeDoc struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
	Text  string   `json:"text"`
}

type claim struct {
	Text    string   `json:"text"`
	Support []string `json:"support"`
}

type knowledgeCase struct {
	ID       string   `json:"id"`
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
	Claims   []claim  `json:"claims"`
	KeyTerms []string `json:"key_terms"`
}

type caseMetrics struct {
	CaseID      string   `json:"case_id"`
	Config      string   `json:"config"`
	Retrieved   []string `json:"retrieved"`
	Precision   float64  `json:"context_precision"`
	Faithful    float64  `json:"faithfulness"`
	Relevancy   float64  `json:"answer_relevancy"`
	Answer      string   `json:"answer"`
	Missing     []string `json:"missing_terms,omitempty"`
	Unsupported []string `json:"unsupported_claims,omitempty"`
}

type configResult struct {
	Name    string        `json:"name"`
	Case    []caseMetrics `json:"cases"`
	MeanP   float64       `json:"mean_context_precision"`
	MeanF   float64       `json:"mean_faithfulness"`
	MeanR   float64       `json:"mean_relevancy"`
	MeanAll float64       `json:"mean_overall"`
}

type knowledgeReport struct {
	Dataset     string         `json:"dataset"`
	Docs        int            `json:"docs"`
	Cases       int            `json:"cases"`
	MinScore    float64        `json:"min_score"`
	MaxRegress  float64        `json:"max_regress"`
	Runs        []configResult `json:"runs"`
	Regressions []string       `json:"regressions"`
	Pass        bool           `json:"pass"`
}

// retrieveConfig describes one retrieval strategy. The two configurations are
// the "previous run" and the "current run" compared in the report.
type retrieveConfig struct {
	name     string
	seeds    int
	maxDocs  int
	neighbor bool // expand one hop through shared tags, mirroring graph expansion
}

var kbConfigs = []retrieveConfig{
	{name: "run1-baseline-lexical", seeds: 3, maxDocs: 3},
	{name: "run2-seed+neighbor", seeds: 2, maxDocs: 4, neighbor: true},
}

func runKnowledge(args []string) int {
	fs := flag.NewFlagSet("knowledge", flag.ExitOnError)
	root := fs.String("root", filepath.Join("bench", "parser", "knowledge"), "dataset directory")
	minScore := fs.Float64("min-score", 0.40, "absolute floor applied to every mean metric of the last run")
	maxRegress := fs.Float64("max-regress", 0.05, "allowed drop of a mean metric between the two runs")
	jsonOut := fs.String("json", "", "write the machine readable report to this path")
	_ = fs.Parse(args)

	docs, err := loadDocs(filepath.Join(*root, "docs.jsonl"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load docs: %v\n", err)
		return 2
	}
	cases, err := loadCases(filepath.Join(*root, "cases.jsonl"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load cases: %v\n", err)
		return 2
	}

	corpus := newCorpus(docs)
	rep := knowledgeReport{Dataset: *root, Docs: len(docs), Cases: len(cases), MinScore: *minScore, MaxRegress: *maxRegress}
	for _, cfg := range kbConfigs {
		rep.Runs = append(rep.Runs, corpus.evaluate(cases, cfg))
	}

	base, current := rep.Runs[0], rep.Runs[len(rep.Runs)-1]
	rep.Regressions = diffRuns(base, current)
	floorOK := current.MeanP+1e-9 >= *minScore && current.MeanF+1e-9 >= *minScore && current.MeanR+1e-9 >= *minScore
	driftOK := base.MeanP-current.MeanP <= *maxRegress+1e-9 &&
		base.MeanF-current.MeanF <= *maxRegress+1e-9 &&
		base.MeanR-current.MeanR <= *maxRegress+1e-9
	rep.Pass = floorOK && driftOK

	printKnowledge(rep)
	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, rep); err != nil {
			fmt.Fprintf(os.Stderr, "write json: %v\n", err)
			return 2
		}
	}
	if !rep.Pass {
		if !floorOK {
			fmt.Printf("\nFAIL: the last run is below the %.2f absolute floor on at least one metric\n", *minScore)
		}
		if !driftOK {
			fmt.Printf("\nFAIL: a mean metric dropped by more than %.2f between the two runs\n", *maxRegress)
		}
		return 1
	}
	fmt.Printf("\nPASS: above the %.2f floor and no metric dropped by more than %.2f\n", *minScore, *maxRegress)
	return 0
}

func loadDocs(path string) ([]knowledgeDoc, error) {
	var out []knowledgeDoc
	err := eachJSONLine(path, func(raw []byte) error {
		var doc knowledgeDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		out = append(out, doc)
		return nil
	})
	return out, err
}

func loadCases(path string) ([]knowledgeCase, error) {
	var out []knowledgeCase
	err := eachJSONLine(path, func(raw []byte) error {
		var item knowledgeCase
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		out = append(out, item)
		return nil
	})
	return out, err
}

func eachJSONLine(path string, fn func([]byte) error) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "//") {
			continue
		}
		if err := fn([]byte(text)); err != nil {
			return fmt.Errorf("%s line %d: %w", filepath.Base(path), line, err)
		}
	}
	return scanner.Err()
}

type scoredDoc struct {
	doc   knowledgeDoc
	score float64
}

type corpusIndex struct {
	docs []knowledgeDoc
	idf  map[string]float64
	toks map[string]map[string]int // doc id -> token -> tf
	norm map[string]float64
	tags map[string][]string // tag -> doc ids
}

var latinToken = regexp.MustCompile(`[a-z0-9_]+`)

func newCorpus(docs []knowledgeDoc) *corpusIndex {
	idx := &corpusIndex{
		docs: docs,
		idf:  map[string]float64{},
		toks: map[string]map[string]int{},
		norm: map[string]float64{},
		tags: map[string][]string{},
	}
	df := map[string]int{}
	for _, doc := range docs {
		counts := tokenCounts(doc.Title + " " + doc.Text)
		idx.toks[doc.ID] = counts
		norm := 0.0
		for _, tf := range counts {
			norm += float64(tf * tf)
		}
		idx.norm[doc.ID] = math.Sqrt(norm)
		for token := range counts {
			df[token]++
		}
		for _, tag := range doc.Tags {
			idx.tags[tag] = append(idx.tags[tag], doc.ID)
		}
	}
	for token, count := range df {
		idx.idf[token] = math.Log(1 + float64(len(docs))/float64(count))
	}
	return idx
}

// tokenCounts mixes latin words with CJK character bigrams: no dictionary is
// involved, so the same text always yields the same tokens.
func tokenCounts(text string) map[string]int {
	lower := strings.ToLower(text)
	out := map[string]int{}
	for _, word := range latinToken.FindAllString(lower, -1) {
		out[word]++
	}
	runes := []rune(lower)
	var prev rune
	have := false
	for _, r := range runes {
		if !isCJK(r) {
			have = false
			continue
		}
		if have {
			out[string([]rune{prev, r})]++
		}
		prev, have = r, true
	}
	return out
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF)
}

func (c *corpusIndex) rank(query string) []scoredDoc {
	q := tokenCounts(query)
	out := make([]scoredDoc, 0, len(c.docs))
	for _, doc := range c.docs {
		counts := c.toks[doc.ID]
		score := 0.0
		for token := range q {
			if tf, ok := counts[token]; ok && c.norm[doc.ID] > 0 {
				score += float64(tf) * c.idf[token]
			}
		}
		if c.norm[doc.ID] > 0 {
			score /= c.norm[doc.ID]
		}
		if score > 0 {
			out = append(out, scoredDoc{doc: doc, score: score})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score == out[j].score {
			return out[i].doc.ID < out[j].doc.ID
		}
		return out[i].score > out[j].score
	})
	return out
}

func (c *corpusIndex) retrieve(query string, cfg retrieveConfig) []string {
	ranked := c.rank(query)
	if len(ranked) > cfg.seeds {
		ranked = ranked[:cfg.seeds]
	}
	picked := make([]string, 0, cfg.maxDocs)
	seen := map[string]bool{}
	for _, item := range ranked {
		picked = append(picked, item.doc.ID)
		seen[item.doc.ID] = true
	}
	if !cfg.neighbor {
		return picked
	}
	for _, seed := range ranked {
		for _, tag := range seed.doc.Tags {
			for _, id := range c.tags[tag] {
				if seen[id] || len(picked) >= cfg.maxDocs {
					continue
				}
				picked = append(picked, id)
				seen[id] = true
			}
		}
	}
	if len(picked) > cfg.maxDocs {
		picked = picked[:cfg.maxDocs]
	}
	return picked
}

func (c *corpusIndex) evaluate(cases []knowledgeCase, cfg retrieveConfig) configResult {
	out := configResult{Name: cfg.name}
	for _, item := range cases {
		retrieved := c.retrieve(item.Query, cfg)
		metrics := caseMetrics{CaseID: item.ID, Config: cfg.name, Retrieved: retrieved}
		metrics.Precision = contextPrecision(retrieved, item.Relevant)
		metrics.Faithful, metrics.Unsupported = faithfulness(item.Claims, retrieved)
		metrics.Answer = c.extractiveAnswer(item.Query, retrieved)
		metrics.Relevancy, metrics.Missing = relevancy(item.KeyTerms, metrics.Answer)
		out.Case = append(out.Case, metrics)
	}
	out.MeanP, out.MeanF, out.MeanR = mean(out.Case, func(m caseMetrics) float64 { return m.Precision }),
		mean(out.Case, func(m caseMetrics) float64 { return m.Faithful }),
		mean(out.Case, func(m caseMetrics) float64 { return m.Relevancy })
	out.MeanAll = (out.MeanP + out.MeanF + out.MeanR) / 3
	return out
}

func contextPrecision(retrieved, relevant []string) float64 {
	if len(retrieved) == 0 {
		return 0
	}
	relevantSet := toSet(relevant)
	hits := 0
	for _, id := range retrieved {
		if relevantSet[id] {
			hits++
		}
	}
	return float64(hits) / float64(len(retrieved))
}

// faithfulness counts a claim as supported when at least one of the documents
// that state it survived retrieval: the metric therefore reacts to recall loss.
func faithfulness(claims []claim, retrieved []string) (float64, []string) {
	if len(claims) == 0 {
		return 1, nil
	}
	retrievedSet := toSet(retrieved)
	supported := 0
	var unsupported []string
	for _, item := range claims {
		ok := false
		for _, docID := range item.Support {
			if retrievedSet[docID] {
				ok = true
				break
			}
		}
		if ok {
			supported++
			continue
		}
		unsupported = append(unsupported, item.Text)
	}
	return float64(supported) / float64(len(claims)), unsupported
}

func relevancy(keyTerms []string, answer string) (float64, []string) {
	if len(keyTerms) == 0 {
		return 1, nil
	}
	lower := strings.ToLower(answer)
	hit := 0
	var missing []string
	for _, term := range keyTerms {
		if strings.Contains(lower, strings.ToLower(term)) {
			hit++
			continue
		}
		missing = append(missing, term)
	}
	return float64(hit) / float64(len(keyTerms)), missing
}

var sentenceSplit = regexp.MustCompile(`[。！？\n]+`)

// extractiveAnswer is the declared stand-in for an LLM: it takes the two
// retrieved sentences with the largest query overlap.
func (c *corpusIndex) extractiveAnswer(query string, retrieved []string) string {
	q := tokenCounts(query)
	type candidate struct {
		text  string
		score float64
		order int
	}
	var pool []candidate
	order := 0
	for _, id := range retrieved {
		doc, ok := c.docByID(id)
		if !ok {
			continue
		}
		for _, sentence := range sentenceSplit.Split(doc.Text, -1) {
			text := strings.TrimSpace(sentence)
			if text == "" {
				continue
			}
			counts := tokenCounts(text)
			score := 0.0
			for token := range q {
				if counts[token] > 0 {
					score++
				}
			}
			pool = append(pool, candidate{text: text, score: score, order: order})
			order++
		}
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].score > pool[j].score })
	const take = 2
	var parts []string
	for i, item := range pool {
		if i >= take {
			break
		}
		parts = append(parts, item.text)
	}
	return strings.Join(parts, "。")
}

func (c *corpusIndex) docByID(id string) (knowledgeDoc, bool) {
	for _, doc := range c.docs {
		if doc.ID == id {
			return doc, true
		}
	}
	return knowledgeDoc{}, false
}

// diffRuns reports the cases whose metrics fell between the two runs, which is
// how a regression is located instead of only observed in the mean.
func diffRuns(base, current configResult) []string {
	byID := map[string]caseMetrics{}
	for _, item := range base.Case {
		byID[item.CaseID] = item
	}
	var out []string
	for _, item := range current.Case {
		prev, ok := byID[item.CaseID]
		if !ok {
			continue
		}
		if item.Precision+1e-9 < prev.Precision {
			out = append(out, fmt.Sprintf("%s: context_precision %.3f -> %.3f (retrieved %v -> %v)",
				item.CaseID, prev.Precision, item.Precision, prev.Retrieved, item.Retrieved))
		}
		if item.Faithful+1e-9 < prev.Faithful {
			out = append(out, fmt.Sprintf("%s: faithfulness %.3f -> %.3f (retrieved %v -> %v)",
				item.CaseID, prev.Faithful, item.Faithful, prev.Retrieved, item.Retrieved))
		}
		if item.Relevancy+1e-9 < prev.Relevancy {
			out = append(out, fmt.Sprintf("%s: answer_relevancy %.3f -> %.3f (missing %v)",
				item.CaseID, prev.Relevancy, item.Relevancy, item.Missing))
		}
	}
	return out
}

func printKnowledge(rep knowledgeReport) {
	fmt.Printf("knowledge evaluation (deterministic, human annotated)\n")
	fmt.Printf("dataset=%s docs=%d cases=%d floor=%.2f max_regress=%.2f\n\n", rep.Dataset, rep.Docs, rep.Cases, rep.MinScore, rep.MaxRegress)
	for _, run := range rep.Runs {
		fmt.Printf("[%s]\n", run.Name)
		fmt.Printf("  %-9s %-24s %-9s %-9s %-10s %s\n", "case", "retrieved", "precision", "faithful", "relevancy", "missing terms")
		for _, item := range run.Case {
			fmt.Printf("  %-9s %-24s %-9.3f %-9.3f %-10.3f %s\n",
				item.CaseID, shorten(strings.Join(item.Retrieved, ","), 24), item.Precision, item.Faithful, item.Relevancy,
				strings.Join(item.Missing, ","))
		}
		fmt.Printf("  mean: precision=%.3f faithfulness=%.3f relevancy=%.3f overall=%.3f\n\n",
			run.MeanP, run.MeanF, run.MeanR, run.MeanAll)
	}
	base, current := rep.Runs[0], rep.Runs[len(rep.Runs)-1]
	fmt.Printf("delta %s -> %s: precision %+.3f faithfulness %+.3f relevancy %+.3f\n",
		base.Name, current.Name, current.MeanP-base.MeanP, current.MeanF-base.MeanF, current.MeanR-base.MeanR)
	if len(rep.Regressions) == 0 {
		fmt.Printf("regressions: none\n")
		return
	}
	fmt.Printf("regressions (%d):\n", len(rep.Regressions))
	for _, line := range rep.Regressions {
		fmt.Printf("  - %s\n", line)
	}
}

func mean(items []caseMetrics, pick func(caseMetrics) float64) float64 {
	if len(items) == 0 {
		return 0
	}
	total := 0.0
	for _, item := range items {
		total += pick(item)
	}
	return total / float64(len(items))
}

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}
