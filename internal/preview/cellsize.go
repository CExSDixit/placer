package preview

import (
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// Terminal cell size in pixels, which is what decides how much resolution a
// graphics-protocol preview actually gets.
//
// This was guessed at 8×16 and that is wrong by a factor of two on a HiDPI
// display: Ghostty on a Retina screen reports cells around 16×38 physical
// pixels, so an image rendered for 8×16 gets upscaled by the terminal and
// arrives visibly soft — which is exactly the "some images look compressed"
// difference between a photo and a screenshot. Ask the terminal instead.
var (
	cellPxW atomic.Int32
	cellPxH atomic.Int32
)

// Fallback for terminals that report no pixel geometry — every block
// renderer, and a few graphics ones.
const (
	defaultCellPxW = 10
	defaultCellPxH = 20
)

func init() {
	cellPxW.Store(defaultCellPxW)
	cellPxH.Store(defaultCellPxH)
}

// DetectCellPixels asks the terminal for its window size in pixels and
// derives the per-cell size. Call once at startup, next to DetectProtocol.
// Terminals that don't report pixel geometry (Terminal.app) leave the
// defaults in place, which costs nothing: they render blocks, where the cell
// pixel size is fixed by the glyph, not the display.
func DetectCellPixels() {
	for _, f := range []*os.File{os.Stdout, os.Stderr, os.Stdin} {
		ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
		if err != nil || ws.Xpixel == 0 || ws.Ypixel == 0 || ws.Col == 0 || ws.Row == 0 {
			continue
		}
		w, h := int32(ws.Xpixel)/int32(ws.Col), int32(ws.Ypixel)/int32(ws.Row)
		// Sanity-bound it: a bogus report should degrade to the default
		// rather than ask for a 100-megapixel thumbnail.
		if w < 4 || h < 4 || w > 64 || h > 128 {
			continue
		}
		cellPxW.Store(w)
		cellPxH.Store(h)
		return
	}
}

// CellPixels reports the detected per-cell pixel size.
func CellPixels() (int, int) {
	return int(cellPxW.Load()), int(cellPxH.Load())
}
