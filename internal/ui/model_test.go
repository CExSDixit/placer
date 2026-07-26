package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mishrasidhant/placer/internal/device"
	"github.com/mishrasidhant/placer/internal/index"
	"github.com/mishrasidhant/placer/internal/session"
)

// key builds the tea.KeyMsg for a key name as Model.handleKey sees it.
func key(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func press(m Model, keys ...string) Model {
	for _, k := range keys {
		next, _ := m.Update(key(k))
		m = next.(Model)
	}
	return m
}

func typeIn(m Model, s string) Model {
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(Model)
	}
	return m
}

// newTestModel isolates HOME so config/manifest never touch the real cache.
func newTestModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	m := New(device.Synthetic(1))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	next, _ = m.Update(m.loadIndex())
	return next.(Model)
}

func TestIndexLoads(t *testing.T) {
	m := newTestModel(t)
	if m.loading {
		t.Fatal("still loading after indexLoadedMsg")
	}
	c := m.ix.Counts()
	// Synthetic mirrors the measured Pixel 6a distribution.
	if c[index.TabPhotos] < 10000 {
		t.Errorf("photos = %d, want >10000", c[index.TabPhotos])
	}
	if c[index.TabVideo] != 483 {
		t.Errorf("video = %d, want 483", c[index.TabVideo])
	}
	if c[index.TabAudio] != 125 { // 124 + one tricky name
		t.Errorf("audio = %d, want 125", c[index.TabAudio])
	}
}

func TestVimNavigation(t *testing.T) {
	m := newTestModel(t)
	if m.cursor != 0 {
		t.Fatalf("cursor starts at %d", m.cursor)
	}
	m = press(m, "j", "j", "j")
	if m.cursor != 3 {
		t.Errorf("after jjj cursor = %d, want 3", m.cursor)
	}
	m = press(m, "k")
	if m.cursor != 2 {
		t.Errorf("after k cursor = %d, want 2", m.cursor)
	}
	m = press(m, "G")
	if want := len(m.view.Files) - 1; m.cursor != want {
		t.Errorf("after G cursor = %d, want %d", m.cursor, want)
	}
	if m.offset == 0 {
		t.Error("offset should have scrolled after G")
	}
	m = press(m, "g", "g")
	if m.cursor != 0 || m.offset != 0 {
		t.Errorf("after gg cursor=%d offset=%d, want 0/0", m.cursor, m.offset)
	}
	m = press(m, "ctrl+d")
	if m.cursor != m.listHeight()/2 {
		t.Errorf("ctrl+d cursor = %d, want %d", m.cursor, m.listHeight()/2)
	}
	m = press(m, "ctrl+u")
	if m.cursor != 0 {
		t.Errorf("ctrl+u should return to 0, got %d", m.cursor)
	}
	// k at the top must not go negative.
	m = press(m, "k", "k")
	if m.cursor != 0 {
		t.Errorf("cursor went negative: %d", m.cursor)
	}
}

func TestSelectionToggleAdvances(t *testing.T) {
	m := newTestModel(t)
	first := m.view.Files[0]
	m = press(m, "tab")
	if !m.man.Has(first) {
		t.Error("tab did not select the file under the cursor")
	}
	if m.cursor != 1 {
		t.Errorf("tab should advance the cursor, got %d", m.cursor)
	}
	// Toggling the same file off again.
	m = press(m, "k", "tab")
	if m.man.Has(first) {
		t.Error("second tab did not deselect")
	}
}

func TestVisualRangeSelect(t *testing.T) {
	m := newTestModel(t)
	m = press(m, "v", "j", "j", "j") // anchor 0, cursor 3
	if !m.visual {
		t.Fatal("v did not enter visual mode")
	}
	want := m.view.Files[:4]
	m = press(m, "tab")
	if m.visual {
		t.Error("tab should leave visual mode")
	}
	if m.man.Len() != 4 {
		t.Fatalf("selected %d, want 4", m.man.Len())
	}
	for _, f := range want {
		if !m.man.Has(f) {
			t.Errorf("%s not selected", f.Name)
		}
	}
}

func TestVisualRangeBackwards(t *testing.T) {
	m := newTestModel(t)
	m = press(m, "j", "j", "j", "j", "v", "k", "k") // anchor 4, cursor 2
	m = press(m, "tab")
	if m.man.Len() != 3 {
		t.Errorf("backwards range selected %d, want 3", m.man.Len())
	}
}

func TestSearchFiltersAndEscKeepsFilter(t *testing.T) {
	m := newTestModel(t)
	total := len(m.view.Files)

	m = press(m, "/")
	if m.mode != modeSearch {
		t.Fatal("/ did not enter search mode")
	}
	m = typeIn(m, "Screenshots")
	if len(m.view.Files) == 0 {
		t.Fatal("search returned nothing")
	}
	if len(m.view.Files) >= total {
		t.Errorf("search did not narrow: %d of %d", len(m.view.Files), total)
	}
	for _, f := range m.view.Files[:min(5, len(m.view.Files))] {
		if !strings.Contains(f.Bucket, "Screenshots") {
			t.Errorf("unexpected match: %s in %s", f.Name, f.Bucket)
		}
	}

	filtered := len(m.view.Files)
	m = press(m, "esc")
	if m.mode != modeNormal {
		t.Error("esc did not return to normal mode")
	}
	if len(m.view.Files) != filtered {
		t.Error("esc should keep the filter")
	}
	// A second esc in normal mode clears it.
	m = press(m, "esc")
	if len(m.view.Files) != total {
		t.Errorf("esc in normal mode should clear filter: %d, want %d", len(m.view.Files), total)
	}
}

// The property that justifies a manifest: selection is independent of tab and
// filter, so curation survives navigation.
func TestSelectionSurvivesTabAndFilter(t *testing.T) {
	m := newTestModel(t)
	picked := m.view.Files[0]
	m = press(m, "tab")

	m = press(m, "3") // audio tab
	if m.tab != index.TabAudio {
		t.Fatalf("tab = %v", m.tab)
	}
	if !m.man.Has(picked) {
		t.Error("selection lost when switching tabs")
	}
	m = press(m, "1") // back to photos
	m = press(m, "/")
	m = typeIn(m, "zzzzznomatch")
	if !m.man.Has(picked) {
		t.Error("selection lost when filtering")
	}
	m = press(m, "esc", "esc")
	if !m.man.Has(picked) {
		t.Error("selection lost when clearing filter")
	}
	if m.man.Len() != 1 {
		t.Errorf("manifest size drifted: %d", m.man.Len())
	}
}

func TestTabCyclingWithGt(t *testing.T) {
	m := newTestModel(t)
	m = press(m, "g", "t")
	if m.tab != index.TabVideo {
		t.Errorf("gt -> %v, want Video", m.tab)
	}
	m = press(m, "g", "T")
	if m.tab != index.TabPhotos {
		t.Errorf("gT -> %v, want Photos", m.tab)
	}
}

func TestSortCommand(t *testing.T) {
	m := newTestModel(t)
	m = press(m, ":")
	m = typeIn(m, "sort size")
	next, _ := m.Update(key("enter"))
	m = next.(Model)

	if m.sortBy != index.SortSize {
		t.Fatalf("sortBy = %v", m.sortBy)
	}
	files := m.view.Files
	for i := 1; i < min(20, len(files)); i++ {
		if files[i-1].Size < files[i].Size {
			t.Errorf("not sorted by size desc at %d: %d < %d", i, files[i-1].Size, files[i].Size)
			break
		}
	}
}

func TestSortByNameAndDate(t *testing.T) {
	m := newTestModel(t)
	m = press(m, ":")
	m = typeIn(m, "sort name")
	next, _ := m.Update(key("enter"))
	m = next.(Model)
	files := m.view.Files
	for i := 1; i < min(20, len(files)); i++ {
		if strings.ToLower(files[i-1].Name) > strings.ToLower(files[i].Name) {
			t.Errorf("not sorted by name at %d: %q > %q", i, files[i-1].Name, files[i].Name)
			break
		}
	}

	m = press(m, ":")
	m = typeIn(m, "sort date")
	next, _ = m.Update(key("enter"))
	m = next.(Model)
	files = m.view.Files
	for i := 1; i < min(20, len(files)); i++ {
		if files[i-1].SortTime().Before(files[i].SortTime()) {
			t.Error("not sorted newest-first")
			break
		}
	}
}

func TestSelectAllVisible(t *testing.T) {
	m := newTestModel(t)
	m = press(m, "3") // audio: small enough to check exactly
	n := len(m.view.Files)
	m = press(m, "V")
	if m.man.Len() != n {
		t.Errorf("V selected %d, want %d", m.man.Len(), n)
	}
}

func TestSelectionPaneRemoves(t *testing.T) {
	m := newTestModel(t)
	m = press(m, "tab", "tab", "tab") // select 3
	if m.man.Len() != 3 {
		t.Fatalf("setup selected %d", m.man.Len())
	}
	m = press(m, "s")
	if m.mode != modeSelection {
		t.Fatal("s did not open the selection pane")
	}
	m = press(m, "d")
	if m.man.Len() != 2 {
		t.Errorf("d removed %d, manifest = %d", 1, m.man.Len())
	}
	m = press(m, "c")
	if m.man.Len() != 0 {
		t.Errorf("c did not clear: %d", m.man.Len())
	}
	m = press(m, "esc")
	if m.mode != modeNormal {
		t.Error("esc did not leave the selection pane")
	}
}

func TestQuitGuardsUnsavedSelection(t *testing.T) {
	m := newTestModel(t)
	m = press(m, "tab")
	next, _ := m.Update(key("q"))
	m = next.(Model)
	if m.quit {
		t.Error("q quit despite a non-empty selection")
	}
	if !strings.Contains(m.status, "selected") {
		t.Errorf("expected a warning, got %q", m.status)
	}
	// :q! discards and quits.
	m = press(m, ":")
	m = typeIn(m, "q!")
	next, _ = m.Update(key("enter"))
	m = next.(Model)
	if !m.quit {
		t.Error(":q! did not quit")
	}
}

func TestManifestPersistsAcrossSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	m := New(device.Synthetic(1))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	next, _ = m.Update(m.loadIndex())
	m = next.(Model)
	m = press(m, "tab", "tab")
	picked := m.man.Files()
	if err := m.man.Save(session.ManifestPath()); err != nil {
		t.Fatal(err)
	}

	reloaded := session.LoadManifest(session.ManifestPath())
	if reloaded.Len() != 2 {
		t.Fatalf("reloaded %d, want 2", reloaded.Len())
	}
	for _, f := range picked {
		if !reloaded.Has(f) {
			t.Errorf("%s missing after reload", f.Name)
		}
	}
}

func TestViewRendersInAllModes(t *testing.T) {
	m := newTestModel(t)
	for _, keys := range [][]string{
		{}, {"/"}, {":"}, {"?"}, {"s"}, {"d"}, {"v"}, {"3"}, {"5"},
	} {
		mm := press(m, keys...)
		out := mm.View()
		if out == "" {
			t.Errorf("empty view after %v", keys)
		}
		if strings.Contains(out, "%!") {
			t.Errorf("format verb error after %v:\n%s", keys, out)
		}
	}
}

func TestNarrowTerminalDoesNotPanic(t *testing.T) {
	m := newTestModel(t)
	for _, w := range []int{20, 40, 10} {
		next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 8})
		mm := next.(Model)
		if mm.View() == "" {
			t.Errorf("empty view at width %d", w)
		}
	}
}

func TestShortKind(t *testing.T) {
	cases := map[string]string{
		"image/x-adobe-dng": "dng",
		"image/svg+xml":     "svg",
		"image/jpeg":        "jpeg",
		"audio/x-wav":       "wav",
		"video/mp4":         "mp4",
		"application/pdf":   "pdf",
	}
	for mime, want := range cases {
		got := shortKind(device.File{Mime: mime})
		if got != want {
			t.Errorf("shortKind(%q) = %q, want %q", mime, got, want)
		}
	}
	// Timed media shows duration instead.
	if got := shortKind(device.File{Mime: "video/mp4", Duration: 95 * time.Second}); got != "1:35" {
		t.Errorf("duration label = %q, want 1:35", got)
	}
}

// Regression: with wide tab labels the header status cluster must degrade
// gracefully instead of being chopped mid-word by the terminal, and no header
// line may exceed the terminal width.
func TestHeaderNeverOverflows(t *testing.T) {
	m := newTestModel(t)
	for _, w := range []int{200, 120, 90, 79, 70, 60, 40, 24} {
		next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		mm := next.(Model)
		for i, line := range strings.Split(mm.header(), "\n") {
			if got := lipgloss.Width(line); got > w {
				t.Errorf("width %d: header line %d is %d wide:\n%q", w, i, got, line)
			}
		}
	}
}

// At the width from the real screenshot the serial must be whole or absent —
// never a fragment like "23311".
func TestHeaderSerialNotTruncatedMidWord(t *testing.T) {
	m := newTestModel(t)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 79, Height: 30})
	mm := next.(Model)
	line := strings.Split(mm.header(), "\n")[0]
	serial := mm.dev.Serial()
	for n := 4; n < len(serial); n++ {
		frag := serial[:n]
		if strings.Contains(line, frag) && !strings.Contains(line, serial) {
			t.Errorf("header shows serial fragment %q without the full serial:\n%q", frag, line)
			return
		}
	}
}

// Panes that print their own hints must not also get the list footer hint.
func TestNoDuplicateHintLines(t *testing.T) {
	m := newTestModel(t)
	for _, keys := range [][]string{{"tab", "s"}, {"d"}} {
		mm := press(m, keys...)
		out := mm.View()
		if n := strings.Count(out, "j/k move"); n > 1 {
			t.Errorf("after %v: %d hint lines containing \"j/k move\":\n%s", keys, n, out)
		}
	}
}
