package preview

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/CExSDixit/placer/internal/device"
)

// TestQuadrantDoublesHorizontalResolution is the point of the whole renderer:
// for the same pane, quadrant blocks carry 2×2 source pixels per cell where
// half-blocks carry 1×2.
func TestQuadrantDoublesHorizontalResolution(t *testing.T) {
	hx, hy := ProtoHalfBlock.subcell()
	qx, qy := ProtoQuadrant.subcell()
	if hx != 1 || hy != 2 {
		t.Errorf("half-block subcell = %dx%d, want 1x2", hx, hy)
	}
	if qx != 2 || qy != 2 {
		t.Errorf("quadrant subcell = %dx%d, want 2x2", qx, qy)
	}

	// A wide source into a 40x20 pane: quadrant asks for twice the pixels.
	hw, _ := targetPixels(4000, 3000, 40, 20, ProtoHalfBlock)
	qw, _ := targetPixels(4000, 3000, 40, 20, ProtoQuadrant)
	if qw <= hw {
		t.Errorf("quadrant target width %d is not greater than half-block's %d", qw, hw)
	}
}

// TestGraphicsProtocolsGetRealResolution: the graphics protocols were being
// handed a source only cellW pixels wide — ~48 px for a whole photo — and
// then asked to scale it up to fill the pane, throwing away almost everything
// before the terminal ever saw it.
func TestGraphicsProtocolsGetRealResolution(t *testing.T) {
	blockW, _ := targetPixels(4000, 3000, 48, 30, ProtoQuadrant)
	kittyW, _ := targetPixels(4000, 3000, 48, 30, ProtoKitty)
	if kittyW < blockW*3 {
		t.Errorf("kitty target width %d is barely above the block renderer's %d — "+
			"a graphics protocol should get a real image", kittyW, blockW)
	}
	if kittyW > maxPreviewDim {
		t.Errorf("kitty target width %d exceeds the %d cap", kittyW, maxPreviewDim)
	}
}

func TestTargetPixelsNeverUpscales(t *testing.T) {
	// A 16x16 icon into a big pane stays 16x16 rather than being blown up
	// into a soft mess.
	w, h := targetPixels(16, 16, 100, 50, ProtoKitty)
	if w > 16+2 || h > 16+2 {
		t.Errorf("targetPixels upscaled a 16x16 source to %dx%d", w, h)
	}
}

func TestTargetPixelsAlignsToSubcellGrid(t *testing.T) {
	for _, p := range []Protocol{ProtoHalfBlock, ProtoQuadrant} {
		sx, sy := p.subcell()
		w, h := targetPixels(1997, 1333, 41, 23, p)
		if w%sx != 0 || h%sy != 0 {
			t.Errorf("%s: %dx%d does not align to the %dx%d subcell grid", p, w, h, sx, sy)
		}
	}
}

// TestQuadrantRendersSolidAndSplitCells checks the partition search picks the
// obviously-right glyph in the two unambiguous cases.
func TestQuadrantRendersSolidAndSplitCells(t *testing.T) {
	solid := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			solid.Set(x, y, color.RGBA{200, 100, 50, 255})
		}
	}
	// A uniform cell has zero error for every mask; whichever wins, fg and bg
	// are the same colour, so it must not look like anything but a flat block.
	got := quadrantRender(solid)
	if !strings.ContainsAny(got, string(quadrantChars[:])) {
		t.Errorf("uniform cell produced no block glyph: %q", got)
	}

	// Left half black, right half white — the unambiguous ▌ / ▐ case.
	split := image.NewRGBA(image.Rect(0, 0, 2, 2))
	split.Set(0, 0, color.RGBA{0, 0, 0, 255})
	split.Set(0, 1, color.RGBA{0, 0, 0, 255})
	split.Set(1, 0, color.RGBA{255, 255, 255, 255})
	split.Set(1, 1, color.RGBA{255, 255, 255, 255})
	got = quadrantRender(split)
	if !strings.ContainsRune(got, '▌') && !strings.ContainsRune(got, '▐') {
		t.Errorf("a left/right split did not render as a vertical half: %q", got)
	}
}

func TestQuadrantRenderRowCount(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	rows := strings.Split(quadrantRender(img), "\n")
	if len(rows) != 3 { // 6 pixel rows / 2 per cell
		t.Errorf("6 pixel rows produced %d character rows, want 3", len(rows))
	}
}

func TestProtocolIsText(t *testing.T) {
	for _, p := range []Protocol{ProtoHalfBlock, ProtoQuadrant} {
		if !p.IsText() {
			t.Errorf("%s should render as inline text", p)
		}
	}
	for _, p := range []Protocol{ProtoKitty, ProtoIterm, ProtoSixel} {
		if p.IsText() {
			t.Errorf("%s emits graphics escapes and must be placed by the overlay", p)
		}
	}
}

func TestParseProtocol(t *testing.T) {
	for in, want := range map[string]Protocol{
		"quadrant": ProtoQuadrant, "quad": ProtoQuadrant,
		"halfblock": ProtoHalfBlock, "half": ProtoHalfBlock,
		"kitty": ProtoKitty, "iterm": ProtoIterm, "sixel": ProtoSixel,
	} {
		got, ok := ParseProtocol(in)
		if !ok || got != want {
			t.Errorf("ParseProtocol(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := ParseProtocol("ascii"); ok {
		t.Error("ParseProtocol accepted an unknown name")
	}
	// Round-trips through String(), which is what config.json stores.
	for _, p := range []Protocol{ProtoHalfBlock, ProtoQuadrant, ProtoKitty, ProtoIterm, ProtoSixel} {
		if got, ok := ParseProtocol(p.String()); !ok || got != p {
			t.Errorf("%v did not round-trip through its name", p)
		}
	}
}

// TestProtocolIsInTheCacheKey: switching renderer must not serve the old
// renderer's bytes.
func TestProtocolIsInTheCacheKey(t *testing.T) {
	f := sampleFileStub()
	if cacheKey(f, 40, 20, ProtoHalfBlock) == cacheKey(f, 40, 20, ProtoQuadrant) {
		t.Error("half-block and quadrant renders share a cache key")
	}
}

func sampleFileStub() device.File {
	return device.File{Path: "/sdcard/a.jpg", Size: 1000, Mime: "image/jpeg"}
}
