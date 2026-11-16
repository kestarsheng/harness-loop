// Package benchsample holds parser accuracy fixtures for the Go language.
package benchsample

import (
	"fmt"
	"sort"
	"strings"
)

// Task is one unit of work tracked by the loop.
type Task struct {
	ID       string
	Priority int
	Tags     []string
}

// Filter keeps the tasks carrying tag, ordered by priority descending.
func Filter(tasks []Task, tag string) []Task {
	out := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		if HasTag(task, tag) {
			out = append(out, task)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Priority > out[j].Priority
	})
	return out
}

// HasTag reports whether the task carries tag, ignoring case.
func HasTag(task Task, tag string) bool {
	for _, item := range task.Tags {
		if strings.EqualFold(item, tag) {
			return true
		}
	}
	return false
}

// Summary renders a single-line summary of the task.
func (t Task) Summary() string {
	return fmt.Sprintf("%s(%d) %s", t.ID, t.Priority, strings.Join(t.Tags, ","))
}
