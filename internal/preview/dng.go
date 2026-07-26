package preview

import (
	"encoding/binary"
	"errors"
)

// DNG is a TIFF container. Measured against a real Pixel 6a DNG: neither IFD0
// nor its SubIFD use the classic EXIF JPEGInterchangeFormat tag pair (0x0201/
// 0x0202) — instead each is Compression=7 ("new-style JPEG") and stores the
// embedded preview as a single TIFF strip, addressed by the ordinary
// StripOffsets(0x0111)/StripByteCounts(0x0117) tags, which for a single-strip
// JPEG-compressed IFD is a complete standalone JPEG stream (verified:
// FFD8...FFD9). Higher-resolution DNG previews are often *tiled* JPEG
// instead (TileOffsets/TileByteCounts, tag 0x0144/0x0145) with a shared
// header split across tiles — reconstructing that is real work for no
// benefit here, so it's skipped; the single-strip preview (1280×720 on the
// reference device) is already far more than a terminal cell needs.
//
// We walk IFD0, every SubIFD (tag 0x014A) and the Exif IFD (tag 0x8769),
// and keep the largest single-strip JPEG found.
const (
	tagSubIFDs        = 0x014A
	tagExifIFD        = 0x8769
	tagCompression    = 0x0103
	tagStripOffsets   = 0x0111
	tagStripByteCount = 0x0117

	compressionOldJPEG = 6
	compressionNewJPEG = 7

	typeShort = 3
	typeLong  = 4
)

var errNotTIFF = errors.New("not a TIFF/DNG container")

// ExtractDNGPreview walks a DNG's IFD tree and returns the largest embedded
// JPEG preview it can find, pure Go, no raw decoding.
func ExtractDNGPreview(data []byte) ([]byte, error) {
	if len(data) < 8 {
		return nil, errNotTIFF
	}
	var order binary.ByteOrder
	switch string(data[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil, errNotTIFF
	}
	if order.Uint16(data[2:4]) != 42 {
		return nil, errNotTIFF
	}

	var best []byte
	visited := map[uint32]bool{}
	var walk func(off uint32)
	walk = func(off uint32) {
		for off != 0 && !visited[off] {
			visited[off] = true
			entries, next, ok := readIFD(data, order, off)
			if !ok {
				return
			}
			var compression uint32
			var stripOff, stripLen []uint32
			for _, e := range entries {
				switch e.tag {
				case tagCompression:
					compression = e.asUint32(data, order)
				case tagStripOffsets:
					stripOff = e.asUint32Slice(data, order)
				case tagStripByteCount:
					stripLen = e.asUint32Slice(data, order)
				case tagSubIFDs:
					for _, sub := range e.asUint32Slice(data, order) {
						walk(sub)
					}
				case tagExifIFD:
					walk(e.asUint32(data, order))
				}
			}
			// A single-strip, JPEG-compressed IFD is a complete standalone
			// JPEG stream. Multi-strip or tiled previews are skipped rather
			// than reassembled — see the package doc comment.
			if (compression == compressionOldJPEG || compression == compressionNewJPEG) &&
				len(stripOff) == 1 && len(stripLen) == 1 {
				start, ln := int64(stripOff[0]), int64(stripLen[0])
				if ln > 0 && start+ln <= int64(len(data)) {
					blob := data[start : start+ln]
					if len(blob) > len(best) && isJPEG(blob) {
						best = blob
					}
				}
			}
			off = next
		}
	}
	walk(order.Uint32(data[4:8]))

	if best == nil {
		return nil, errors.New("no embedded JPEG preview found")
	}
	return best, nil
}

func isJPEG(b []byte) bool {
	return len(b) > 2 && b[0] == 0xFF && b[1] == 0xD8
}

type ifdEntry struct {
	tag, typ    uint16
	count       uint32
	valueOffset [4]byte // raw 4-byte value/offset field, endianness applied by caller
}

// asUint32 interprets a single-value entry (SHORT or LONG) as its resolved
// value: inline for count==1, else the value stored at the offset.
func (e ifdEntry) asUint32(data []byte, order binary.ByteOrder) uint32 {
	switch e.typ {
	case typeShort:
		if e.count == 1 {
			return uint32(order.Uint16(e.valueOffset[:2]))
		}
	case typeLong:
		if e.count == 1 {
			return order.Uint32(e.valueOffset[:4])
		}
	}
	off := order.Uint32(e.valueOffset[:4])
	if e.typ == typeShort && int64(off)+2 <= int64(len(data)) {
		return uint32(order.Uint16(data[off : off+2]))
	}
	if int64(off)+4 <= int64(len(data)) {
		return order.Uint32(data[off : off+4])
	}
	return 0
}

// asUint32Slice reads a multi-value LONG/SHORT array (used for SubIFDs,
// which can point at more than one sub-tree).
func (e ifdEntry) asUint32Slice(data []byte, order binary.ByteOrder) []uint32 {
	elemSize := uint32(4)
	if e.typ == typeShort {
		elemSize = 2
	}
	total := elemSize * e.count
	var src []byte
	if total <= 4 {
		src = e.valueOffset[:total]
	} else {
		off := order.Uint32(e.valueOffset[:4])
		if int64(off)+int64(total) > int64(len(data)) {
			return nil
		}
		src = data[off : off+total]
	}
	out := make([]uint32, 0, e.count)
	for i := uint32(0); i < e.count; i++ {
		if e.typ == typeShort {
			out = append(out, uint32(order.Uint16(src[i*2:i*2+2])))
		} else {
			out = append(out, order.Uint32(src[i*4:i*4+4]))
		}
	}
	return out
}

func readIFD(data []byte, order binary.ByteOrder, off uint32) ([]ifdEntry, uint32, bool) {
	if int64(off)+2 > int64(len(data)) {
		return nil, 0, false
	}
	n := int(order.Uint16(data[off : off+2]))
	base := off + 2
	need := int64(base) + int64(n)*12 + 4
	if need > int64(len(data)) {
		return nil, 0, false
	}
	entries := make([]ifdEntry, n)
	for i := 0; i < n; i++ {
		p := base + uint32(i*12)
		e := ifdEntry{
			tag:   order.Uint16(data[p : p+2]),
			typ:   order.Uint16(data[p+2 : p+4]),
			count: order.Uint32(data[p+4 : p+8]),
		}
		copy(e.valueOffset[:], data[p+8:p+12])
		entries[i] = e
	}
	next := order.Uint32(data[base+uint32(n*12) : base+uint32(n*12)+4])
	return entries, next, true
}
