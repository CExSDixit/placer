package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/index"
	"github.com/CExSDixit/placer/internal/preview"
)

// TestRealPreviewFetch runs the phase 2 preview pipeline — pull, decode,
// downscale, render — against one real file of each type on the actual
// device, the same code path the TUI uses on cursor rest.
func TestRealPreviewFetch(t *testing.T) {
	dev := realDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ix, _ := index.Load(ctx, dev)

	pick := map[string]device.File{}
	for _, f := range ix.All {
		switch f.Mime {
		case "image/jpeg", "image/png", "image/x-adobe-dng", "image/heic", "image/heif":
			if _, ok := pick[f.Mime]; !ok && f.Size > 200_000 {
				pick[f.Mime] = f
			}
		}
	}
	if len(pick) == 0 {
		t.Skip("no previewable images found on device")
	}

	proto := preview.DetectProtocol()
	t.Logf("detected terminal protocol: %s", proto)

	for mime, f := range pick {
		f := f
		t.Run(mime, func(t *testing.T) {
			start := time.Now()
			fctx, fcancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer fcancel()
			res, err := preview.Fetch(fctx, dev, f, 40, 20, proto)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Fetch(%s, %s): %v (%s)", mime, f.Name, err, elapsed)
			}
			t.Logf("%s %-40s tier=%v rendered=%dB note=%q (%s)",
				mime, f.Name, res.Tier, len(res.Rendered), res.Note, elapsed)
			if res.Tier != preview.TierMeta && len(res.Rendered) == 0 {
				t.Errorf("%s: non-metadata tier produced no rendered bytes", mime)
			}
		})
	}
}
