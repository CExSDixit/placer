package ui

import (
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
