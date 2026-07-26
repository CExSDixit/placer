package preview

import (
	"context"
	"fmt"
	"os"

	"github.com/CExSDixit/placer/internal/device"
)

// Tier is which preview strategy applied to a file, per the spec's per-type
// table.
type Tier int

const (
	TierImage Tier = iota // jpeg/png/gif — full stdlib decode, 99.4% of the library
	TierDNG               // embedded JPEG extracted from the TIFF container
	TierMeta              // heic and everything else — metadata card, no bytes fetched
)

// Result is what Fetch hands back to the UI: either rendered image bytes
// ready to draw, or a reason there's nothing to render.
type Result struct {
	Tier     Tier
	Rendered []byte // protocol-ready bytes for TierImage/TierDNG
	Note     string // e.g. "heic — metadata only", or an error summary
}

// Fetch pulls, decodes, downscales, renders and caches a preview for f. It
// respects ctx cancellation at every blocking step (the pull, decode and
// render all happen after checks), so a cursor move that cancels ctx stops
// the fetch promptly rather than finishing a now-stale request.
func Fetch(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	switch f.Kind() {
	case device.KindImage:
		return fetchImage(ctx, dev, f, cellW, cellH, proto)
	default:
		return Result{Tier: TierMeta, Note: "no preview for this file type"}, nil
	}
}

func fetchImage(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	tier := TierImage
	switch {
	case mimeIs(f.Mime, "image/x-adobe-dng"):
		tier = TierDNG
	case mimeIs(f.Mime, "image/heic"), mimeIs(f.Mime, "image/heif"):
		// No pure-Go decoder and not worth a dependency for 3 files on the
		// reference device — metadata card only, per spec.
		return Result{Tier: TierMeta, Note: "heic — metadata only"}, nil
	case !mimeIs(f.Mime, "image/jpeg") && !mimeIs(f.Mime, "image/png") && !mimeIs(f.Mime, "image/gif"):
		return Result{Tier: TierMeta, Note: "unsupported image type"}, nil
	}

	if cached, ok := readCache(f, cellW, cellH, proto); ok {
		return Result{Tier: tier, Rendered: cached}, nil
	}

	data, err := pullBytes(ctx, dev, f)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	decodeMime := f.Mime
	if tier == TierDNG {
		jpg, err := ExtractDNGPreview(data)
		if err != nil {
			return Result{Tier: TierMeta, Note: "dng — no embedded preview: " + err.Error()}, nil
		}
		data = jpg
		decodeMime = "image/jpeg" // ExtractDNGPreview always yields a JPEG
	}

	img, err := decodeStdlib(decodeMime, data)
	if err != nil {
		return Result{}, fmt.Errorf("decode %s: %w", f.Name, err)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	small := downscale(img, cellW, cellH)
	rendered, err := Render(small, proto, cellW, cellH)
	if err != nil {
		return Result{}, fmt.Errorf("render %s: %w", f.Name, err)
	}

	writeCache(f, cellW, cellH, proto, rendered)
	return Result{Tier: tier, Rendered: rendered}, nil
}

// pullBytes fetches the whole file — the EXIF head-bytes fast path was
// measured and cut (see the scope doc): adb pull already lands a 2.8 MB
// photo in ~120 ms, faster than the 89 ms partial fetch could ever save.
func pullBytes(ctx context.Context, dev device.Device, f device.File) ([]byte, error) {
	tmp, err := os.CreateTemp("", "placer-preview-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := dev.Pull(ctx, f.Path, tmpPath, nil); err != nil {
		return nil, err
	}
	return os.ReadFile(tmpPath)
}
