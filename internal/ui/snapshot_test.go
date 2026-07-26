package ui

import (
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestSnapshot renders each screen so a human can eyeball layout.
//
//	go test ./internal/ui -run TestSnapshot -v
func TestSnapshot(t *testing.T) {
	if os.Getenv("PLACER_SNAPSHOT") == "" {
		t.Skip("set PLACER_SNAPSHOT=1 to dump rendered screens")
	}
	base := newTestModel(t)
	next, _ := base.Update(tea.WindowSizeMsg{Width: 110, Height: 22})
	base = next.(Model)

	for _, tc := range []struct {
		name string
		keys []string
	}{
		{"list", nil},
		{"search", []string{"/", "s", "c", "r", "e", "e", "n"}},
		{"visual", []string{"j", "v", "j", "j"}},
		{"selected", []string{"tab", "tab", "tab"}},
		{"audio-tab", []string{"3"}},
		{"selection-pane", []string{"tab", "tab", "s"}},
		{"destination", []string{"d"}},
		{"help", []string{"?"}},
	} {
		m := press(base, tc.keys...)
		t.Logf("\n=== %s ===\n%s\n", tc.name, m.View())
	}
}
