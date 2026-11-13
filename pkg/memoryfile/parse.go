package memoryfile

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func Parse(raw []byte) (*Document, error) {
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return nil, fmt.Errorf("%w: file must start with '---'", ErrInvalidFrontmatter)
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("%w: frontmatter is not closed", ErrInvalidFrontmatter)
	}
	frontmatter := rest[:end]
	body := rest[end+len("\n---\n"):]

	doc := &Document{SchemaVersion: 0}
	if err := parseFrontmatter(frontmatter, doc); err != nil {
		return nil, err
	}

	body = strings.TrimLeft(body, "\n")
	if !strings.HasPrefix(body, "<!-- order:") {
		return nil, fmt.Errorf("%w: order block is missing", ErrInvalidFrontmatter)
	}
	orderEnd := strings.Index(body, "-->")
	if orderEnd < 0 {
		return nil, fmt.Errorf("%w: order block is not closed", ErrInvalidFrontmatter)
	}
	orderLine := strings.TrimSpace(body[len("<!-- order:"):orderEnd])
	var order []string
	if err := json.Unmarshal([]byte(orderLine), &order); err != nil {
		return nil, fmt.Errorf("%w: order is not a json array: %v", ErrInvalidFrontmatter, err)
	}
	doc.Order = order
	body = body[orderEnd+len("-->"):]

	topics, err := parseTopics(body)
	if err != nil {
		return nil, err
	}
	doc.Topics = topics
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

func parseFrontmatter(raw string, doc *Document) error {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("%w: line %q is not key: value", ErrInvalidFrontmatter, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "schema_version":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("%w: schema_version %q", ErrInvalidFrontmatter, value)
			}
			doc.SchemaVersion = v
		case "scope":
			doc.Scope = value
		case "tenant_id":
			doc.TenantID = value
		case "project_id":
			doc.ProjectID = value
		case "updated_at":
			ts, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return fmt.Errorf("%w: updated_at %q", ErrInvalidFrontmatter, value)
			}
			doc.UpdatedAt = ts.UTC()
		case "order_version":
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("%w: order_version %q", ErrInvalidFrontmatter, value)
			}
			doc.OrderVersion = v
		default:
			return fmt.Errorf("%w: unknown frontmatter key %q", ErrInvalidFrontmatter, key)
		}
	}
	return nil
}

func parseTopics(body string) ([]Topic, error) {
	blocks := splitTopicBlocks(body)
	topics := make([]Topic, 0, len(blocks))
	for _, block := range blocks {
		topic, err := parseTopicBlock(block)
		if err != nil {
			return nil, err
		}
		topics = append(topics, topic)
	}
	return topics, nil
}

func splitTopicBlocks(body string) []string {
	lines := strings.Split(body, "\n")
	var (
		blocks  []string
		current []string
	)
	flush := func() {
		if len(current) > 0 {
			blocks = append(blocks, strings.Join(current, "\n"))
			current = nil
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			flush()
			current = []string{line}
			continue
		}
		if current != nil {
			current = append(current, line)
		}
	}
	flush()
	return blocks
}

func parseTopicBlock(block string) (Topic, error) {
	lines := strings.Split(block, "\n")
	if len(lines) < 2 {
		return Topic{}, fmt.Errorf("%w: topic block is too short", ErrInvalidMeta)
	}
	key := strings.TrimSpace(strings.TrimPrefix(lines[0], "## "))
	if !ValidTopicKey(key) {
		return Topic{}, fmt.Errorf("%w: %q", ErrInvalidTopicKey, key)
	}
	metaLine := strings.TrimSpace(lines[1])
	if !strings.HasPrefix(metaLine, "<!-- meta:") || !strings.HasSuffix(metaLine, "-->") {
		return Topic{}, fmt.Errorf("%w: topic %q meta line is missing", ErrInvalidMeta, key)
	}
	metaRaw := strings.TrimSuffix(strings.TrimPrefix(metaLine, "<!-- meta:"), "-->")
	meta, err := parseMeta(metaRaw)
	if err != nil {
		return Topic{}, fmt.Errorf("%w: topic %q: %v", ErrInvalidMeta, key, err)
	}

	rest := strings.Join(lines[2:], "\n")
	rest = strings.TrimLeft(rest, "\n")
	restLines := strings.Split(rest, "\n")
	if len(restLines) == 0 || !strings.HasPrefix(restLines[0], "### ") {
		return Topic{}, fmt.Errorf("%w: topic %q title line is missing", ErrInvalidMeta, key)
	}
	title := strings.TrimSpace(strings.TrimPrefix(restLines[0], "### "))
	body := strings.TrimSpace(strings.Join(restLines[1:], "\n"))

	return Topic{Key: key, Title: title, Body: body, Meta: meta}, nil
}

func parseMeta(raw string) (Meta, error) {
	var meta Meta
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return Meta{}, fmt.Errorf("meta field %q is not key=value", part)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "category":
			meta.Category = Category(v)
			if !meta.Category.Valid() {
				return Meta{}, fmt.Errorf("unknown category %q", v)
			}
		case "pinned":
			meta.Pinned = v == "true"
		case "updated_at":
			ts, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return Meta{}, fmt.Errorf("updated_at %q", v)
			}
			meta.UpdatedAt = ts.UTC()
		case "content_hash":
			meta.ContentHash = v
		default:
			return Meta{}, fmt.Errorf("unknown meta key %q", k)
		}
	}
	return meta, nil
}
