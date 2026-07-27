package preview

import (
	"bytes"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
)

var errUnsupportedFormat = errors.New("preview: unsupported image format")

// maxPreviewDim bounds the decoded/rendered image so a 48 MP photo isn't
// carried around at full resolution; nothing here needs more than a
// graphics-protocol terminal can show in a third of a window.
const maxPreviewDim = 2400

// decodeStdlib handles the 99.4% case: JPEG, PNG, GIF via Go's stdlib.
func decodeStdlib(mime string, data []byte) (image.Image, error) {
	r := bytes.NewReader(data)
	switch {
	case mimeIs(mime, "image/jpeg"):
		return jpeg.Decode(r)
	case mimeIs(mime, "image/png"):
		return png.Decode(r)
	case mimeIs(mime, "image/gif"):
		return gif.Decode(r) // first frame only — this is a still preview
	}
	// Fall back to format sniffing: extension-derived mime types are not
	// always trustworthy, but the magic bytes always are.
	if img, _, err := image.Decode(bytes.NewReader(data)); err == nil {
		return img, nil
	}
	return nil, errUnsupportedFormat
}

func mimeIs(mime, want string) bool {
	return len(mime) >= len(want) && mime[:len(want)] == want
}

// downscale fits img within w×h terminal cells for the given protocol, using
// a CatmullRom filter — there's no fast path worth taking since previews are
// cached after the first decode.
func downscale(img image.Image, cellW, cellH int, proto Protocol) image.Image {
	pxW, pxH := targetPixels(img.Bounds().Dx(), img.Bounds().Dy(), cellW, cellH, proto)
	dst := image.NewRGBA(image.Rect(0, 0, pxW, pxH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	return dst
}

// subcell reports how many source pixels each terminal cell carries, which is
// the entire resolution story for a preview:
//
//	half-block  1×2 — one glyph, foreground over background
//	quadrant    2×2 — twice the horizontal detail for the same pane
//	graphics    the terminal's real pixels-per-cell; the grid stops being the limit
//
// The graphics protocols were once fed a source only cellW pixels wide, i.e.
// ~48 px for a whole photo, and then asked to scale it up to fill the pane —
// which threw away almost everything before the terminal ever saw it. A fixed
// 8×16 was better but still half the truth on a HiDPI display, so the size
// comes from the terminal itself; see DetectCellPixels.
func (p Protocol) subcell() (int, int) {
	switch p {
	case ProtoQuadrant:
		return 2, 2
	case ProtoKitty, ProtoIterm, ProtoSixel:
		return CellPixels()
	default:
		return 1, 2
	}
}

// targetPixels fits the source aspect ratio inside cellW×cellH terminal cells
// at the protocol's pixel density.
func targetPixels(srcW, srcH, cellW, cellH int, proto Protocol) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		srcW, srcH = 1, 1
	}
	sx, sy := proto.subcell()
	maxW := min(cellW*sx, maxPreviewDim)
	maxH := min(cellH*sy, maxPreviewDim)
	scale := min(float64(maxW)/float64(srcW), float64(maxH)/float64(srcH))
	if scale > 1 {
		scale = 1 // never upscale; a small source stays small rather than soft
	}
	w := max(1, int(float64(srcW)*scale))
	h := max(1, int(float64(srcH)*scale))
	// Block rendering maps a whole number of pixel rows/columns per glyph, so
	// round up to the subcell grid rather than dropping a partial row. The
	// graphics protocols have no such constraint — rounding there would
	// upscale a small source for no reason.
	if proto.IsText() {
		for w%sx != 0 {
			w++
		}
		for h%sy != 0 {
			h++
		}
	}
	return w, h
}
