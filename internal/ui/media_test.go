package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/index"
	"github.com/CExSDixit/placer/internal/player"
	"github.com/CExSDixit/placer/internal/preview"
)

// collectDebounce runs the model through a cursor move and returns the
// debounce message it scheduled, which is where the autoplay/audio gating
// decision is recorded.
func debounceFor(t *testing.T, m *Model) (previewDebounceMsg, bool) {
	t.Helper()
	cmd := m.schedulePreview()
	if cmd == nil {
		return previewDebounceMsg{}, false
	}
	msg, ok := cmd().(previewDebounceMsg)
	return msg, ok
}

func toTab(t *testing.T, m Model, tab index.Tab) Model {
	t.Helper()
	m.tab = tab
	m.cursor, m.offset = 0, 0
	m.rebuildView()
	return m
}

// TestVideoAutoplayGatesTheFrameGrab is the setting Sid confirmed: a frame
// grab costs ~1.2s even with the sparse trick, so it must not fire on every
// cursor move. Off means a metadata card built with no device round trip.
func TestVideoAutoplayGatesTheFrameGrab(t *testing.T) {
	m := newTestModel(t)
	m = toTab(t, m, index.TabVideo)
	if f, ok := m.cur(); !ok || f.Kind() != device.KindVideo {
		t.Fatalf("expected a video under the cursor, got %+v", f)
	}

	m.cfg.Autoplay = false
	msg, scheduled := debounceFor(t, &m)
	if scheduled && msg.fetch {
		t.Error("autoplay off must not schedule a video fetch")
	}
	if m.preview.result.Tier != preview.TierMeta {
		t.Errorf("autoplay off should show a metadata card, got tier %v", m.preview.result.Tier)
	}
	if len(m.preview.result.Meta) == 0 {
		t.Error("the metadata card should carry the MediaStore fields")
	}
	if m.preview.loading {
		t.Error("a metadata card needs no fetch, so it must not show as loading")
	}

	m.cfg.Autoplay = true
	msg, scheduled = debounceFor(t, &m)
	if !scheduled || !msg.fetch {
		t.Error("autoplay on must schedule the frame grab")
	}
}

// TestAudioAutoplayIndependentOfPreviewPane: `:set audio on` is the
// scrub-through-voice-memos flow and must work with the preview pane off.
func TestAudioAutoplayIndependentOfPreviewPane(t *testing.T) {
	m := newTestModel(t)
	m = toTab(t, m, index.TabAudio)
	m.player = player.New(func(string) string { return "/usr/bin/true" })

	m.cfg.Preview = false
	m.cfg.Audio = true
	msg, scheduled := debounceFor(t, &m)
	if !scheduled || !msg.audio {
		t.Error("audio autoplay should schedule with the preview pane off")
	}
	if msg.fetch {
		t.Error("no waveform fetch should be scheduled when preview is off")
	}

	m.cfg.Audio = false
	msg, scheduled = debounceFor(t, &m)
	if scheduled && msg.audio {
		t.Error(":set audio off must stop j/k from triggering playback")
	}
}

// TestAudioAutoplayNeedsAPlayer: with no ffplay/mpv/afplay on the box,
// nothing should be scheduled — degrade, never hang or crash.
func TestAudioAutoplayNeedsAPlayer(t *testing.T) {
	m := newTestModel(t)
	m = toTab(t, m, index.TabAudio)
	m.player = player.New(func(string) string { return "" })
	m.cfg.Preview, m.cfg.Audio = false, true

	if msg, scheduled := debounceFor(t, &m); scheduled && msg.audio {
		t.Error("no playback backend means no audio autoplay")
	}
}

// TestTransportKeysAreAudioTabOnly: space keeps meaning "select" everywhere
// else, the binding it has had since phase 1.
func TestTransportKeysAreAudioTabOnly(t *testing.T) {
	m := newTestModel(t)
	m.player = player.New(func(string) string { return "/usr/bin/true" })

	m = toTab(t, m, index.TabPhotos)
	before := m.man.Len()
	m = press(m, "space")
	if m.man.Len() != before+1 {
		t.Error("space in the Photos tab must still toggle selection")
	}

	m = toTab(t, m, index.TabAudio)
	before = m.man.Len()
	m = press(m, "space")
	if m.man.Len() != before {
		t.Error("space in the Audio tab is play/pause, not select")
	}
	// tab still selects there, so the Audio tab is not a dead end for curation.
	m = press(m, "tab")
	if m.man.Len() != before+1 {
		t.Error("tab must still select in the Audio tab")
	}
}

func TestTransportKeysFallThroughWithoutAPlayer(t *testing.T) {
	m := newTestModel(t)
	m = toTab(t, m, index.TabAudio)
	m.player = player.New(func(string) string { return "" })

	before := m.man.Len()
	m = press(m, "space")
	if m.man.Len() != before+1 {
		t.Error("with no playback backend, space should fall through to select")
	}
}

// TestStalePullDoesNotStartPlayback is the audio equivalent of the preview
// pipeline's stale-result guard: an adb pull that lands after the cursor has
// moved must not start playing.
func TestStalePullDoesNotStartPlayback(t *testing.T) {
	m := newTestModel(t)
	m = toTab(t, m, index.TabAudio)
	m.player = player.New(func(string) string { return "/usr/bin/true" })

	f, _ := m.cur()
	stale := audioReadyMsg{gen: m.playGen - 1, file: f, local: "/tmp/nope.mp3"}
	next, _ := m.Update(stale)
	m = next.(Model)
	if k, _ := m.player.Loaded(); k != "" {
		t.Errorf("a stale pull started playback of %q", k)
	}
}

func TestPlayTickStopsWhenPlaybackDoes(t *testing.T) {
	m := newTestModel(t)
	m.player = player.New(func(string) string { return "" })
	// Nothing is playing, so the tick must not reschedule itself forever.
	next, cmd := m.Update(playTickMsg{gen: m.playGen})
	m = next.(Model)
	if cmd != nil {
		t.Error("a tick with nothing playing should not reschedule")
	}
	if _, cmd := m.Update(playTickMsg{gen: m.playGen - 1}); cmd != nil {
		t.Error("a stale tick should not reschedule")
	}
}

// TestSetAudioOffStopsPlayback: nobody should ever have to hold a key down to
// stop something playing.
func TestSetAudioOffStopsPlayback(t *testing.T) {
	m := newTestModel(t)
	m = toTab(t, m, index.TabAudio)
	m.player = player.New(func(string) string { return "" })
	m.playLoad = "Recording_001.wav"

	gen := m.playGen
	next, _ := m.runCommand("set audio off")
	m = next.(Model)
	if m.cfg.Audio {
		t.Error(":set audio off did not clear the setting")
	}
	if m.playGen == gen {
		t.Error(":set audio off must retire any in-flight pull")
	}
	if m.playLoad != "" {
		t.Error(":set audio off must clear the pending-playback indicator")
	}
}

// TestQuitStopsPlayback: an ffplay process must not outlive the TUI.
func TestQuitStopsPlayback(t *testing.T) {
	m := newTestModel(t)
	m.player = player.New(func(string) string { return "" })
	m.playLoad = "Recording_001.wav"
	m = press(m, "ctrl+c")
	if !m.quit {
		t.Fatal("ctrl+c should quit")
	}
	if m.playLoad != "" {
		t.Error("quitting must tear down pending playback")
	}
}

// TestMediaPaneLeavesRoomForMetadata: the image is sized to the rows it will
// actually get, since the cache key includes that geometry.
func TestMediaPaneLeavesRoomForMetadata(t *testing.T) {
	m := newTestModel(t)
	m = toTab(t, m, index.TabAudio)
	audio, _ := m.cur()

	_, fullH := m.previewCellSize()
	_, mediaH := m.previewCellSizeFor(audio)
	if mediaH >= fullH {
		t.Errorf("audio image height %d should leave room below %d for the card", mediaH, fullH)
	}
	if m.overlayRows() != mediaH {
		t.Errorf("overlay reserves %d rows but the fetch was sized at %d", m.overlayRows(), mediaH)
	}

	m = toTab(t, m, index.TabPhotos)
	photo, _ := m.cur()
	if _, h := m.previewCellSizeFor(photo); h != fullH {
		t.Errorf("a photo should get the whole pane: %d vs %d", h, fullH)
	}
}

func TestTransportLineRendersPlayhead(t *testing.T) {
	m := newTestModel(t)
	m.player = player.New(func(string) string { return "" })
	if got := m.transportLine(); got != "" {
		t.Errorf("transport line should be empty with nothing loaded, got %q", got)
	}
	m.playLoad = "Recording_007.wav"
	if got := m.transportLine(); got == "" {
		t.Error("a pending pull should show in the transport line")
	}
	m.playLoad, m.playErr = "", "device offline"
	if got := m.transportLine(); got == "" {
		t.Error("a playback error should surface")
	}
}

func TestSeekStepsMatchTheSpec(t *testing.T) {
	// From the phase 1 normal-mode key table: h/l ±5s, H/L ±30s.
	if seekSmall != 5*time.Second || seekLarge != 30*time.Second {
		t.Errorf("seek steps = %v/%v, want 5s/30s", seekSmall, seekLarge)
	}
}

var _ tea.Msg = playTickMsg{}

// TestGraphicsOverlayErasesOnlyWhenThereIsNothingToDraw: the transmit
// replaces the placement, so a drawing frame must not erase first; a frame
// with no image must, or the previous photo stays on screen.
func TestGraphicsOverlayErasesOnlyWhenThereIsNothingToDraw(t *testing.T) {
	m := newTestModel(t)
	m.proto = preview.ProtoKitty
	f, _ := m.cur()

	m.preview = previewState{}
	if got := m.graphicsOverlay(); !strings.Contains(got, "a=d") {
		t.Errorf("empty preview emitted no delete: %q", got)
	}
	m.preview = previewState{path: previewKeyFor(f), result: preview.MetaCard(f, "video — autoplay off")}
	if got := m.graphicsOverlay(); !strings.Contains(got, "a=d") {
		t.Errorf("metadata card emitted no delete: %q", got)
	}

	m.preview = previewState{
		path:   previewKeyFor(f),
		result: preview.Result{Tier: preview.TierImage, Rendered: []byte("IMAGEBYTES")},
	}
	got := m.graphicsOverlay()
	if strings.Contains(got, "a=d") {
		t.Errorf("a frame that draws also erased: %q", got)
	}
	if !strings.Contains(got, "IMAGEBYTES") {
		t.Errorf("frame does not carry the image: %q", got)
	}
}

// TestGraphicsOverlaySilentForTextRenderers: quadrant/half-block output is
// ordinary text laid out by the pane, so the overlay must stay out of it.
func TestGraphicsOverlaySilentForTextRenderers(t *testing.T) {
	m := newTestModel(t)
	for _, p := range []preview.Protocol{preview.ProtoQuadrant, preview.ProtoHalfBlock} {
		m.proto = p
		if got := m.graphicsOverlay(); got != "" {
			t.Errorf("%s overlay = %q, want empty", p, got)
		}
	}
}

// TestPreviewPaneKeepsFullHeightWhenListIsShort: filtering down to a couple
// of matches must not shrink the preview to a couple of rows. The pane owns
// the body height, not the result set.
func TestPreviewPaneKeepsFullHeightWhenListIsShort(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Preview = true
	if m.previewPaneWidth() <= 0 {
		t.Skip("terminal too narrow in this test model for a preview pane")
	}
	wantLines := m.listHeight() + 1

	full := strings.Split(m.bodyView(), "\n")
	if len(full) != wantLines {
		t.Fatalf("unfiltered body has %d lines, want %d", len(full), wantLines)
	}

	// Filter to something that matches almost nothing.
	m.query = "zzzqqqx"
	m.cursor, m.offset = 0, 0
	m.rebuildView()
	if len(m.view.Files) > 3 {
		t.Fatalf("expected a near-empty result set, got %d files", len(m.view.Files))
	}

	short := strings.Split(m.bodyView(), "\n")
	if len(short) != wantLines {
		t.Errorf("filtered body has %d lines, want %d — the preview pane got truncated",
			len(short), wantLines)
	}
}

// TestClearingSearchKeepsTheCursorOnTheSameFile: after narrowing with `/`,
// picking a file, and pressing esc, you should land on that file's row in the
// full list — not on whatever now happens to occupy the index you were at.
func TestClearingSearchKeepsTheCursorOnTheSameFile(t *testing.T) {
	m := newTestModel(t)
	m = press(m, "/")
	m = typeIn(m, "Screenshots")
	m = press(m, "esc") // leave search mode, filter still applied
	if len(m.view.Files) == 0 {
		t.Skip("no matches for the probe query in the synthetic library")
	}
	m = press(m, "j", "j")

	want, ok := m.cur()
	if !ok {
		t.Fatal("no file under the cursor after filtering")
	}
	filteredIdx := m.cursor

	m = press(m, "esc") // clear the filter
	if m.query != "" {
		t.Fatal("esc did not clear the query")
	}
	got, ok := m.cur()
	if !ok {
		t.Fatal("no file under the cursor after clearing")
	}
	if previewKeyFor(got) != previewKeyFor(want) {
		t.Errorf("cursor landed on %q, want %q", got.Name, want.Name)
	}
	if m.cursor == filteredIdx && len(m.view.Files) > 100 {
		t.Error("cursor kept its index rather than following the file")
	}
	// And the row must be on screen, not scrolled past.
	if m.cursor < m.offset || m.cursor >= m.offset+m.listHeight() {
		t.Errorf("cursor %d is outside the visible window [%d,%d)",
			m.cursor, m.offset, m.offset+m.listHeight())
	}
}

// TestSameFileDoesNotRefetch: preserving the cursor means the previewed file
// is unchanged, so no new fetch should be scheduled.
func TestSameFileDoesNotRefetch(t *testing.T) {
	m := newTestModel(t)
	before := m.curPreviewKey()
	// A key that changes nothing about which file is selected.
	next, cmd := m.Update(key("esc"))
	m = next.(Model)
	if m.curPreviewKey() != before {
		t.Fatal("esc moved the cursor")
	}
	if cmd != nil {
		t.Error("a no-op key scheduled a preview fetch")
	}
}

// TestSavedRenderNeverCrossesTheCapabilityLine covers both directions of the
// same bug: a persisted `:set render` must never claim a capability the
// terminal lacks, nor discard one it has.
func TestSavedRenderNeverCrossesTheCapabilityLine(t *testing.T) {
	// Block choice must not downgrade a graphics terminal. Picking "quadrant"
	// while comparing block renderers in Terminal.app once left Ghostty
	// rendering quadrant forever.
	for _, saved := range []string{"quadrant", "halfblock", "half", "quad"} {
		if got := resolveProtocol(preview.ProtoKitty, saved); got != preview.ProtoKitty {
			t.Errorf("saved %q downgraded a kitty terminal to %v", saved, got)
		}
		if got := resolveProtocol(preview.ProtoIterm, saved); got != preview.ProtoIterm {
			t.Errorf("saved %q downgraded an iterm terminal to %v", saved, got)
		}
	}

	// Graphics choice must not be forced on a terminal with no graphics.
	// A saved "kitty" made placer emit kitty escapes inside a herdr pane,
	// which advertises none and silently swallows them — a blank preview that
	// looked exactly like a herdr session-state problem.
	for _, saved := range []string{"kitty", "iterm", "sixel"} {
		if got := resolveProtocol(preview.ProtoQuadrant, saved); got != preview.ProtoQuadrant {
			t.Errorf("saved %q forced graphics on a block-only terminal: %v", saved, got)
		}
	}

	// Within a class the override is honoured — that is all it is for.
	if got := resolveProtocol(preview.ProtoQuadrant, "halfblock"); got != preview.ProtoHalfBlock {
		t.Errorf("saved halfblock ignored in a block-only terminal: %v", got)
	}
	if got := resolveProtocol(preview.ProtoKitty, "iterm"); got != preview.ProtoIterm {
		t.Errorf("saved iterm ignored in a graphics terminal: %v", got)
	}

	// No override, or an unparseable one, leaves detection alone.
	for _, saved := range []string{"", "nonsense", "auto"} {
		if got := resolveProtocol(preview.ProtoKitty, saved); got != preview.ProtoKitty {
			t.Errorf("saved %q changed the detected protocol to %v", saved, got)
		}
	}
}

// TestSetRenderAutoClearsTheOverride: there has to be a way back to
// autodetection without hand-editing config.json.
func TestSetRenderAutoClearsTheOverride(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Render = "halfblock"

	next, _ := m.runCommand("set render auto")
	m = next.(Model)
	if m.cfg.Render != "" {
		t.Errorf("`:set render auto` left %q saved", m.cfg.Render)
	}
	if m.proto != preview.DetectedProtocol() {
		t.Errorf("proto = %v, want the detected %v", m.proto, preview.DetectedProtocol())
	}
}
