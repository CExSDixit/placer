package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func complete(t *testing.T, m Model, typed string) (string, string) {
	t.Helper()
	return m.completeCommand(typed)
}

func TestCompleteCommandNames(t *testing.T) {
	m := newTestModel(t)

	// One match completes and leaves a trailing space, ready for the argument.
	if got, _ := complete(t, m, "se"); got != "set " {
		t.Errorf(`":se" completed to %q, want "set "`, got)
	}
	if got, _ := complete(t, m, "poli"); got != "policy " {
		t.Errorf(`":poli" completed to %q, want "policy "`, got)
	}

	// Several matches extend as far as they agree and list the rest.
	got, hint := complete(t, m, "buck")
	if got != "bucket" {
		t.Errorf(`":buck" completed to %q, want the common prefix "bucket"`, got)
	}
	if hint == "" {
		t.Error("an ambiguous completion should list the candidates")
	}

	// Nothing matches: leave the line alone rather than eating the input.
	if got, _ := complete(t, m, "zzz"); got != "zzz" {
		t.Errorf("unmatched completion changed the line to %q", got)
	}
}

func TestCompleteCommandArguments(t *testing.T) {
	m := newTestModel(t)

	if got, hint := complete(t, m, "sort "); got != "sort " || hint == "" {
		t.Errorf(`":sort <tab>" = %q hint %q, want the three sort modes listed`, got, hint)
	}
	if got, _ := complete(t, m, "sort d"); got != "sort date " {
		t.Errorf(`":sort d" completed to %q, want "sort date "`, got)
	}
	if got, _ := complete(t, m, "policy ov"); got != "policy overwrite " {
		t.Errorf(`":policy ov" completed to %q`, got)
	}
	if got, _ := complete(t, m, "set au"); got != "set au" {
		t.Errorf(`":set au" is ambiguous (audio/autoplay), got %q`, got)
	}
	if got, _ := complete(t, m, "set aut"); got != "set autoplay " {
		t.Errorf(`":set aut" completed to %q, want "set autoplay "`, got)
	}
	// The value slot completes on|off.
	if got, _ := complete(t, m, "set preview o"); got != "set preview o" {
		t.Errorf(`"o" is ambiguous between on and off, got %q`, got)
	}
	if got, _ := complete(t, m, "set preview of"); got != "set preview off " {
		t.Errorf(`":set preview of" completed to %q`, got)
	}
}

// TestCompleteBucketsFromLiveIndex: bucket names are not a fixed list, they
// come from whatever albums the attached device actually has.
func TestCompleteBucketsFromLiveIndex(t *testing.T) {
	m := newTestModel(t)
	m.tab = 0 // Photos
	m.rebuildView()

	got, _ := complete(t, m, "bucket Cam")
	if got != "bucket Camera " {
		t.Errorf(`":bucket Cam" completed to %q, want "bucket Camera "`, got)
	}
	if got, _ := complete(t, m, "bucket cle"); got != "bucket clear " {
		t.Errorf(`":bucket cle" completed to %q`, got)
	}
}

// TestCompletionDoesNotFireOutsideCommandMode guards the binding: tab is the
// select key in normal mode and must stay that way.
func TestCompletionDoesNotFireOutsideCommandMode(t *testing.T) {
	m := newTestModel(t)
	before := m.man.Len()
	m = press(m, "tab")
	if m.man.Len() != before+1 {
		t.Error("tab in normal mode must still toggle selection")
	}
}

func TestCommandHistoryWalksBothWays(t *testing.T) {
	m := newTestModel(t)
	m = press(m, ":")
	m = typeIn(m, "sort name")
	m = press(m, "enter")
	m = press(m, ":")
	m = typeIn(m, "sort size")
	m = press(m, "enter")

	m = press(m, ":")
	m = typeIn(m, "half typ")

	// Up walks back through history, newest first.
	m = press(m, "up")
	if got := m.input.Value(); got != "sort size" {
		t.Errorf("first up = %q, want the most recent command", got)
	}
	m = press(m, "up")
	if got := m.input.Value(); got != "sort name" {
		t.Errorf("second up = %q", got)
	}
	// Past the oldest entry it stops rather than wrapping.
	m = press(m, "up", "up", "up")
	if got := m.input.Value(); got != "sort name" {
		t.Errorf("walking past the oldest entry = %q", got)
	}
	// Down returns, and the half-typed line comes back at the end.
	m = press(m, "down")
	if got := m.input.Value(); got != "sort size" {
		t.Errorf("down = %q", got)
	}
	m = press(m, "down")
	if got := m.input.Value(); got != "half typ" {
		t.Errorf("arrowing forward past the newest entry = %q, want the draft back", got)
	}
}

func TestCommandHistorySkipsBlanksAndRepeats(t *testing.T) {
	m := newTestModel(t)
	run := func(line string) {
		m = press(m, ":")
		m = typeIn(m, line)
		m = press(m, "enter")
	}
	run("sort name")
	run("sort name")
	run("")
	run("   ")
	if len(m.cmdHistory) != 1 {
		t.Errorf("history = %q, want one entry", m.cmdHistory)
	}
}

func TestCommandHistoryIsSessionOnly(t *testing.T) {
	// A fresh model starts with nothing — the history is deliberately not
	// persisted; `:` commands here aren't expensive enough to warrant it.
	m := newTestModel(t)
	if len(m.cmdHistory) != 0 {
		t.Errorf("a new session started with history: %q", m.cmdHistory)
	}
}

func TestHistoryArrowsAreCommandModeOnly(t *testing.T) {
	m := newTestModel(t)
	m = press(m, ":")
	m = typeIn(m, "sort size")
	m = press(m, "enter")

	// In normal mode, up/down move the cursor as ever.
	start := m.cursor
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.cursor != start+1 {
		t.Errorf("down in normal mode moved the cursor to %d, want %d", m.cursor, start+1)
	}
}
