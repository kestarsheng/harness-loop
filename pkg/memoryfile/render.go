package memoryfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (d *Document) Render() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.WriteString(fmt.Sprintf("schema_version: %d\n", d.SchemaVersion))
	buf.WriteString(fmt.Sprintf("scope: %s\n", d.Scope))
	if d.TenantID != "" {
		buf.WriteString(fmt.Sprintf("tenant_id: %s\n", d.TenantID))
	}
	if d.ProjectID != "" {
		buf.WriteString(fmt.Sprintf("project_id: %s\n", d.ProjectID))
	}
	buf.WriteString(fmt.Sprintf("updated_at: %s\n", d.UpdatedAt.UTC().Format(time.RFC3339)))
	buf.WriteString(fmt.Sprintf("order_version: %d\n", d.OrderVersion))
	buf.WriteString("---\n\n")

	orderJSON, err := json.Marshal(d.Order)
	if err != nil {
		return nil, fmt.Errorf("memoryfile: encode order: %w", err)
	}
	buf.WriteString("<!-- order: ")
	buf.Write(orderJSON)
	buf.WriteString(" -->\n")

	for _, topic := range d.Topics {
		buf.WriteString("\n")
		buf.WriteString("## ")
		buf.WriteString(topic.Key)
		buf.WriteString("\n")
		buf.WriteString(fmt.Sprintf("<!-- meta: category=%s; pinned=%t; updated_at=%s; content_hash=%s -->\n",
			topic.Meta.Category, topic.Meta.Pinned, topic.Meta.UpdatedAt.UTC().Format(time.RFC3339), topic.Meta.ContentHash))
		buf.WriteString("\n")
		buf.WriteString("### ")
		buf.WriteString(topic.Title)
		buf.WriteString("\n")
		buf.WriteString(topic.Body)
		buf.WriteString("\n")
	}
	return buf.Bytes(), nil
}

func (d *Document) RenderString() (string, error) {
	raw, err := d.Render()
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (d *Document) Reorder(keys []string) error {
	if len(keys) != len(d.Topics) {
		return fmt.Errorf("%w: reorder expects %d keys, got %d", ErrOrderMismatch, len(d.Topics), len(keys))
	}
	byKey := make(map[string]Topic, len(d.Topics))
	for _, t := range d.Topics {
		byKey[t.Key] = t
	}
	ordered := make([]Topic, 0, len(keys))
	for _, key := range keys {
		t, ok := byKey[key]
		if !ok {
			return fmt.Errorf("%w: unknown topic %q", ErrOrderMismatch, key)
		}
		ordered = append(ordered, t)
	}
	d.Topics = ordered
	d.Order = append([]string(nil), keys...)
	return nil
}

func (d *Document) UpsertTopic(topic Topic, front bool) {
	for i, existing := range d.Topics {
		if existing.Key == topic.Key {
			d.Topics[i] = topic
			if front {
				d.MoveToFront(topic.Key)
			}
			return
		}
	}
	if front {
		d.Topics = append([]Topic{topic}, d.Topics...)
		d.Order = append([]string{topic.Key}, d.Order...)
		return
	}
	d.Topics = append(d.Topics, topic)
	d.Order = append(d.Order, topic.Key)
}

func (d *Document) MoveToFront(key string) {
	idx := keyIndex(d.Topics, key)
	if idx < 0 {
		return
	}
	topic := d.Topics[idx]
	d.Topics = append(d.Topics[:idx], d.Topics[idx+1:]...)
	d.Topics = append([]Topic{topic}, d.Topics...)
	for i, k := range d.Order {
		if k == key {
			d.Order = append(d.Order[:i], d.Order[i+1:]...)
			break
		}
	}
	d.Order = append([]string{key}, d.Order...)
}

func (d *Document) RemoveTopic(key string) bool {
	idx := keyIndex(d.Topics, key)
	if idx < 0 {
		return false
	}
	d.Topics = append(d.Topics[:idx], d.Topics[idx+1:]...)
	for i, k := range d.Order {
		if k == key {
			d.Order = append(d.Order[:i], d.Order[i+1:]...)
			break
		}
	}
	return true
}

func (d *Document) TopicKeys() []string {
	keys := make([]string, len(d.Topics))
	for i, t := range d.Topics {
		keys[i] = t.Key
	}
	return keys
}

func JoinBody(title, body string) string {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" {
		return body
	}
	return title + "\n" + body
}
