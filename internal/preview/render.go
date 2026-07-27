package preview

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"strings"
	"sync/atomic"

	"github.com/BourgeoisBear/rasterm"
)

// Protocol is the terminal image protocol placer will render through.
// Detected once at startup — see DetectProtocol — never per-frame, since the
// kitty/iterm checks are just environment sniffing but the sixel check does
// a real terminal round-trip and must not run concurrently with Bubble Tea
// reading stdin.
type Protocol int

const (
	ProtoHalfBlock Protocol = iota // "▀" — 1×2 pixels per cell, works literally everywhere
	ProtoSixel
	ProtoIterm
	ProtoKitty
	ProtoQuadrant // 2×2 pixels per cell via quadrant blocks — twice the detail
)

func (p Protocol) String() string {
	switch p {
	case ProtoKitty:
		return "kitty"
	case ProtoIterm:
		return "iterm"
	case ProtoSixel:
		return "sixel"
	case ProtoQuadrant:
		return "quadrant"
	}
	return "halfblock"
}

// ParseProtocol resolves the names `:set render` accepts.
func ParseProtocol(s string) (Protocol, bool) {
	switch s {
	case "halfblock", "half":
		return ProtoHalfBlock, true
	case "quadrant", "quad":
		return ProtoQuadrant, true
	case "kitty":
		return ProtoKitty, true
	case "iterm":
		return ProtoIterm, true
	case "sixel":
		return ProtoSixel, true
	}
	return 0, false
}

// IsText reports whether this protocol's output is ordinary printable
// characters the pane can lay out inline, rather than graphics escapes that
// have to be placed by absolute cursor position (see ui.graphicsOverlay).
func (p Protocol) IsText() bool {
	return p == ProtoHalfBlock || p == ProtoQuadrant
}

// DetectProtocol picks the richest protocol the terminal advertises. Must be
// called before the Bubble Tea program starts reading stdin (rasterm's sixel
// probe puts the terminal in raw mode and reads the response itself).
//
// Ghostty/kitty/WezTerm speak the Kitty graphics protocol; iTerm2/WezTerm
// speak the iTerm2 protocol; Terminal.app speaks neither (measured:
// `kitty:false sixel:false iterm:false`).
//
// Detection is environment-based on purpose. Actively probing the terminal
// with a kitty query (`a=q`) was tried and reverted: inside herdr the query is
// answered `OK`, but the image data never arrives — a `herdr pane read
// --format ansi` of a pane running placer contains every SGR colour escape and
// *zero* APC graphics commands. So the probe reports kitty where kitty does
// not work, which is strictly worse than the environment check: that correctly
// reports "no graphics" in a herdr pane and yields quadrant previews that
// actually render. `:set render kitty` forces it for anyone whose multiplexer
// does pass graphics through.
//
// The no-protocol fallback is quadrant blocks, not half-blocks: same
// compatibility — they are legacy code-page glyphs present in every
// monospace font — for twice the horizontal resolution. `:set render
// halfblock` goes back if a font renders them with gaps.
func DetectProtocol() Protocol {
	if InMultiplexerWithoutGraphics() {
		detected.Store(int32(ProtoQuadrant))
		return ProtoQuadrant
	}
	p := ProtoQuadrant
	switch {
	case rasterm.IsKittyCapable():
		p = ProtoKitty
	case rasterm.IsItermCapable():
		p = ProtoIterm
	default:
		if ok, err := rasterm.IsSixelCapable(); err == nil && ok {
			p = ProtoSixel
		}
	}
	detected.Store(int32(p))
	return p
}

// InMultiplexerWithoutGraphics reports whether placer is running inside a
// terminal multiplexer that does not forward graphics escapes.
//
// This has to be checked BEFORE the environment sniffing, because a
// multiplexer leaks the host terminal's identity into its panes while
// swallowing the graphics that identity implies. Measured for herdr: a pane
// launched from Ghostty reports TERM_PROGRAM=ghostty, so rasterm calls it
// kitty-capable — but `herdr pane read --format ansi` of a pane running placer
// contains every SGR colour escape and *zero* APC graphics commands, and the
// preview pane is simply blank. Launch the same herdr from Terminal.app and
// panes report Apple_Terminal, detection falls back to blocks, and previews
// render fine. That difference is the whole bug: it made the failure look like
// terminal session state rather than detection.
//
// tmux is included by analogy, not measurement: it drops APC by default and
// needs `allow-passthrough on`. Anyone whose multiplexer does forward graphics
// can say so with `-render kitty`.
func InMultiplexerWithoutGraphics() bool {
	return os.Getenv("HERDR_PANE_ID") != "" || os.Getenv("HERDR_ENV") != "" ||
		os.Getenv("TMUX") != ""
}

// detected remembers what DetectProtocol found, so a later `:set render auto`
// can go back to it without re-running the sixel probe — which reads from the
// terminal and must not run while Bubble Tea owns stdin.
var detected atomic.Int32

// DetectedProtocol reports what the terminal advertised at startup,
// regardless of any override in force.
func DetectedProtocol() Protocol { return Protocol(detected.Load()) }

// One image id and one placement id, reused for every preview, so a new
// transmit REPLACES the previous placement instead of stacking another on top
// of it. That is what fixes previews piling up without deleting and redrawing
// on every repaint.
//
// `q=2` on every command is load-bearing, and was established by probing
// Ghostty directly rather than read from the spec: a graphics command carrying
// an image id is acknowledged on stdin — `<ESC>_Gi=7301,p=1;OK<ESC>\` — and
// stdin is where Bubble Tea reads keystrokes, so without it every repaint
// feeds an APC sequence into the key parser. Measured: no id → no reply; an id
// → a reply; an id plus `q=2` → no reply.
const (
	kittyImageID     = 7301
	kittyPlacementID = 1
)

// KittyClear removes placer's placement and frees its data. Emitted only on a
// frame with no image to draw — switching from a photo to a metadata card, for
// instance — because kitty images are persistent graphics that ordinary text
// redraw does not erase. A frame that draws needs no erase: the transmit
// replaces the placement.
func KittyClear() string {
	return fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,p=%d,q=2\x1b\\", kittyImageID, kittyPlacementID)
}

// kittyQuiet inserts `q=2` into the transmit header rasterm builds, which
// otherwise has no way to set it.
func kittyQuiet(payload []byte) []byte {
	const from = "\x1b_Ga=T,"
	const to = "\x1b_Ga=T,q=2,"
	if bytes.HasPrefix(payload, []byte(from)) {
		return append([]byte(to), payload[len(from):]...)
	}
	return payload
}

// Render encodes img for the given protocol at cellW×cellH terminal cells.
// The returned bytes are ready to write straight into the TUI frame.
func Render(img image.Image, proto Protocol, cellW, cellH int) ([]byte, error) {
	switch proto {
	case ProtoKitty:
		var buf bytes.Buffer
		// Fixed ids so each transmit replaces the previous placement, plus
		// q=2 so the terminal does not acknowledge it into Bubble Tea's
		// stdin. Both are load-bearing; see the constants above.
		err := rasterm.KittyWriteImage(&buf, img, rasterm.KittyImgOpts{
			DstCols: uint32(cellW), DstRows: uint32(cellH),
			ImageId: kittyImageID, PlacementId: kittyPlacementID,
		})
		return kittyQuiet(buf.Bytes()), err
	case ProtoIterm:
		var buf bytes.Buffer
		err := rasterm.ItermWriteImageWithOptions(&buf, img, rasterm.ItermImgOpts{
			Width: fmt.Sprintf("%d", cellW), Height: fmt.Sprintf("%d", cellH),
			DisplayInline: true,
		})
		return buf.Bytes(), err
	case ProtoSixel:
		pal := toPaletted(img)
		var buf bytes.Buffer
		err := rasterm.SixelWriteImage(&buf, pal)
		return buf.Bytes(), err
	case ProtoQuadrant:
		return []byte(quadrantRender(img)), nil
	default:
		return []byte(halfBlockRender(img)), nil
	}
}

// quadrantChars maps a 4-bit mask of "which of the cell's 2×2 pixels take the
// foreground colour" to the glyph that draws it. Bit 0 is top-left, 1
// top-right, 2 bottom-left, 3 bottom-right.
//
// These are the legacy quadrant blocks from the DOS-era code page, so they
// are present in essentially every monospace font — including Terminal.app's,
// which advertises no image protocol at all.
var quadrantChars = [16]rune{
	' ', '▘', '▝', '▀',
	'▖', '▌', '▞', '▛',
	'▗', '▚', '▐', '▜',
	'▄', '▙', '▟', '█',
}

// quadrantRender draws img at 2×2 pixels per character cell, twice the
// horizontal detail of the half-block renderer for the same pane.
//
// A cell can carry only two colours, so for each 2×2 block it tries all 16
// foreground/background partitions, takes the mean of each side, and keeps
// whichever split has the least squared error. That is the whole trick: with
// four pixels the search is exhaustive and still trivial.
func quadrantRender(img image.Image) string {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var out strings.Builder

	var px [4][3]float64
	for y := 0; y < h; y += 2 {
		for x := 0; x < w; x += 2 {
			for i, d := range [4][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
				sx, sy := min(x+d[0], w-1), min(y+d[1], h-1)
				r, g, bl, _ := img.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
				px[i] = [3]float64{float64(r >> 8), float64(g >> 8), float64(bl >> 8)}
			}

			bestMask, bestErr := 0, math.Inf(1)
			var bestFg, bestBg [3]float64
			for mask := 0; mask < 16; mask++ {
				fg, bg, nf, nb := [3]float64{}, [3]float64{}, 0, 0
				for i := 0; i < 4; i++ {
					if mask&(1<<i) != 0 {
						addTo(&fg, px[i])
						nf++
					} else {
						addTo(&bg, px[i])
						nb++
					}
				}
				divBy(&fg, nf)
				divBy(&bg, nb)

				var e float64
				for i := 0; i < 4; i++ {
					if mask&(1<<i) != 0 {
						e += sqDist(px[i], fg)
					} else {
						e += sqDist(px[i], bg)
					}
				}
				if e < bestErr {
					bestMask, bestErr, bestFg, bestBg = mask, e, fg, bg
				}
			}

			fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm%c",
				int(bestFg[0]), int(bestFg[1]), int(bestFg[2]),
				int(bestBg[0]), int(bestBg[1]), int(bestBg[2]),
				quadrantChars[bestMask])
		}
		out.WriteString("\x1b[0m\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

func addTo(dst *[3]float64, v [3]float64) {
	for i := range dst {
		dst[i] += v[i]
	}
}

func divBy(dst *[3]float64, n int) {
	if n == 0 {
		return
	}
	for i := range dst {
		dst[i] /= float64(n)
	}
}

func sqDist(a, b [3]float64) float64 {
	var s float64
	for i := range a {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

func toPaletted(img image.Image) *image.Paletted {
	if p, ok := img.(*image.Paletted); ok {
		return p
	}
	b := img.Bounds()
	pal := image.NewPaletted(b, palette256())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			pal.Set(x, y, img.At(x, y))
		}
	}
	return pal
}

// palette256 is a coarse uniform RGB cube, good enough for a "which file is
// this" thumbnail; sixel terminals are rare enough on placer's two target
// platforms that this doesn't need to be pretty.
func palette256() color.Palette {
	var p color.Palette
	for r := 0; r < 6; r++ {
		for g := 0; g < 6; g++ {
			for b := 0; b < 6; b++ {
				p = append(p, color.RGBA{
					R: uint8(r * 51), G: uint8(g * 51), B: uint8(b * 51), A: 255,
				})
			}
		}
	}
	return p
}

// halfBlockRender draws img using "▀" per character cell: the foreground
// color is the top source pixel, the background the bottom one, so one
// character row carries two pixel rows. This is the fallback every terminal
// supports, including Sid's default Terminal.app.
func halfBlockRender(img image.Image) string {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var out strings.Builder
	for y := 0; y < h; y += 2 {
		for x := 0; x < w; x++ {
			tr, tg, tb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			var br, bg, bb uint32
			if y+1 < h {
				br, bg, bb, _ = img.At(b.Min.X+x, b.Min.Y+y+1).RGBA()
			} else {
				br, bg, bb = tr, tg, tb
			}
			fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀",
				tr>>8, tg>>8, tb>>8, br>>8, bg>>8, bb>>8)
		}
		out.WriteString("\x1b[0m\n")
	}
	return strings.TrimRight(out.String(), "\n")
}
