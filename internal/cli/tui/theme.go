package tui

import "agent-platform/internal/cli/tui/views"

type Theme = views.Theme

func NewTheme(noColor bool) Theme { return views.NewTheme(noColor) }
