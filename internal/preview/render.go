package preview

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"strings"

	"github.com/BourgeoisBear/rasterm"
)

// Protocol is the terminal image protocol placer will render through.
// Detected once at startup — see DetectProtocol — never per-frame, since the
// kitty/iterm checks are just environment sniffing but the sixel check does
// a real terminal round-trip and must not run concurrently with Bubble Tea
// reading stdin.
type Protocol int

const (
	ProtoHalfBlock Protocol = iota // works everywhere; ~2 pixel rows per cell
	ProtoSixel
	ProtoIterm
	ProtoKitty
)

func (p Protocol) String() string {
	switch p {
	case ProtoKitty:
		return "kitty"
	case ProtoIterm:
		return "iterm"
	case ProtoSixel:
		return "sixel"
	}
	return "halfblock"
}

// DetectProtocol picks the richest protocol the terminal advertises. Must be
// called before the Bubble Tea program starts reading stdin (rasterm's sixel
// probe puts the terminal in raw mode and reads the response itself).
//
// Ghostty/kitty/WezTerm speak the Kitty graphics protocol; iTerm2/WezTerm
// speak the iTerm2 protocol; Terminal.app speaks neither (measured:
// `kitty:false sixel:false iterm:false`), so it lands on the half-block
// fallback — fine for "is this the right photo", the only job a preview here
// has to do.
func DetectProtocol() Protocol {
	if rasterm.IsKittyCapable() {
		return ProtoKitty
	}
	if rasterm.IsItermCapable() {
		return ProtoIterm
	}
	if ok, err := rasterm.IsSixelCapable(); err == nil && ok {
		return ProtoSixel
	}
	return ProtoHalfBlock
}

// Render encodes img for the given protocol at cellW×cellH terminal cells.
// The returned bytes are ready to write straight into the TUI frame.
func Render(img image.Image, proto Protocol, cellW, cellH int) ([]byte, error) {
	switch proto {
	case ProtoKitty:
		var buf bytes.Buffer
		err := rasterm.KittyWriteImage(&buf, img, rasterm.KittyImgOpts{
			DstCols: uint32(cellW), DstRows: uint32(cellH),
		})
		return buf.Bytes(), err
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
	default:
		return []byte(halfBlockRender(img)), nil
	}
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
