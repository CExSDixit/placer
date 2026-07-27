package ui

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/index"
	"github.com/CExSDixit/placer/internal/preview"
)

// Frame-level tests: drive the real model with a real image through the real
// render path and inspect the bytes View() produces.
//
// This is the layer that was missing. The preview pipeline had unit tests and
// the renderer had wire-format tests, but nothing asserted that the *frame*
// carries the image — which is why "previews render nothing in Ghostty" could
// happen twice with a green suite.

// kittyModel builds a model backed by testdata/sample, positions the cursor on
// a real JPEG, and drives the debounce → fetch → result cycle to completion.
func kittyModel(t *testing.T, proto preview.Protocol) Model {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "sample")
	name := "PXL_20260404_154433428.PORTRAIT.ORIGINAL.jpg"
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Skipf("sample %s not present: %v", name, err)
	}

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, ".cache"))

	dev := &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}
	f := device.File{Path: name, Name: name, Size: fi.Size(),
		Mime: "image/jpeg", Coll: device.Images, Added: time.Unix(1750000000, 0)}

	m := New(dev, proto)
	m.proto = proto // New may apply a saved override; the test pins the renderer
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	m.loading = false
	m.ix = index.NewFrom([]device.File{f})
	m.rebuildView()

	if got, ok := m.cur(); !ok || got.Name != name {
		t.Fatalf("cursor is not on the sample file: %+v", got)
	}

	// Run the fetch synchronously rather than waiting on the debounce tick.
	cellW, cellH := m.previewCellSizeFor(f)
	if cellW == 0 {
		t.Fatal("no preview pane at 120x40 — the test model is too small")
	}
	res, err := preview.FetchAt(context.Background(), dev, f, cellW, cellH, proto, preview.DefaultFrameSeek)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(res.Rendered) == 0 {
		t.Fatal("render produced no bytes")
	}
	m.preview = previewState{path: previewKeyFor(f), result: res}
	return m
}

var apcRe = regexp.MustCompile(`\x1b_G[^\x1b]*\x1b\\`)

// TestFrameCarriesTheKittyImage is the test whose absence let previews break
// in Ghostty twice. It asserts the frame actually contains the transmit, in
// the right order relative to the erase, positioned inside the preview pane.
func TestFrameCarriesTheKittyImage(t *testing.T) {
	m := kittyModel(t, preview.ProtoKitty)
	frame := m.View()

	cmds := apcRe.FindAllString(frame, -1)
	if len(cmds) == 0 {
		t.Fatal("frame contains no kitty graphics command at all — the image is never sent")
	}

	// A frame that draws must not erase first: the placement id makes the
	// transmit replace whatever was there, so erasing would delete and redraw
	// the image on every repaint.
	for _, c := range cmds {
		if strings.Contains(c, "a=d") {
			t.Errorf("a frame that draws also erases: %q", c)
		}
	}
	var sawTransmit bool
	for _, c := range cmds {
		if strings.Contains(c, "a=T") {
			sawTransmit = true
		}
	}
	if !sawTransmit {
		t.Error("frame erases the pane but never transmits a new image")
	}
	// The image must be positioned into the preview pane, not left wherever
	// the renderer's cursor happened to be.
	pw := m.previewPaneWidth()
	wantCol := (m.w - pw - 1) + 2
	if !strings.Contains(frame, "\x1b[4;"+itoa(wantCol)+"H") {
		t.Errorf("frame does not position the image at row 4, col %d", wantCol)
	}
	// And the cursor must be saved/restored around it, or the TUI's own
	// cursor tracking is left pointing into the pane.
	if !strings.Contains(frame, "\x1b[s") || !strings.Contains(frame, "\x1b[u") {
		t.Error("image placement is not wrapped in save/restore cursor")
	}
}

// TestFrameHasNoGraphicsCommandsForBlockRenderers: quadrant/half-block output
// is ordinary text zipped into the pane, and a stray APC sequence would
// corrupt every width calculation the layout depends on.
func TestFrameHasNoGraphicsCommandsForBlockRenderers(t *testing.T) {
	for _, p := range []preview.Protocol{preview.ProtoQuadrant, preview.ProtoHalfBlock} {
		m := kittyModel(t, p)
		frame := m.View()
		if apcRe.MatchString(frame) {
			t.Errorf("%s frame contains a graphics command", p)
		}
		// The block art itself must be in the frame.
		if !strings.Contains(frame, "\x1b[38;2;") {
			t.Errorf("%s frame carries no truecolor block art", p)
		}
	}
}

// TestFrameEraseSurvivesAnEmptyPreview: switching to a file with no image
// must erase, or the previous image stays on screen under the metadata card.
func TestFrameEraseSurvivesAnEmptyPreview(t *testing.T) {
	m := kittyModel(t, preview.ProtoKitty)
	f, _ := m.cur()
	m.preview = previewState{path: previewKeyFor(f), result: preview.MetaCard(f, "no preview")}

	frame := m.View()
	cmds := apcRe.FindAllString(frame, -1)
	if len(cmds) != 1 || !strings.Contains(cmds[0], "a=d") {
		t.Errorf("expected exactly one erase and no transmit, got %d commands: %q", len(cmds), cmds)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
