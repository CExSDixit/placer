package e2e

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/index"
	"github.com/CExSDixit/placer/internal/preview"
)

// largestVideo finds the biggest video on the device — the worst case for a
// frame grab, and the one the sparse reconstruction exists for.
func largestVideo(t *testing.T, dev device.Device) device.File {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ix, _ := index.Load(ctx, dev)

	var big device.File
	for _, f := range ix.All {
		if f.Kind() == device.KindVideo && f.Size > big.Size {
			big = f
		}
	}
	if big.Size == 0 {
		t.Skip("no videos on device")
	}
	return big
}

// TestRealSparseFrameGrabBeatsFullPull is the measurement the whole video
// preview design rests on, re-run against whatever is currently the largest
// video: the library only grows, and a claim from a previous phase is not
// evidence about today's file. It asserts both halves — that the sparse path
// is much faster, and that the frame it yields is the frame a full pull
// would have given.
func TestRealSparseFrameGrabBeatsFullPull(t *testing.T) {
	dev := realDevice(t)
	if !preview.HasFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	f := largestVideo(t, dev)
	t.Logf("largest video: %s (%d MB, %s)", f.Name, f.Size>>20, f.Duration)

	proto := preview.ProtoHalfBlock
	dir := t.TempDir()
	t.Setenv("HOME", dir) // keep the thumb cache out of the real one
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, ".cache"))

	// Sparse: head + tail only, straight through the production Fetch path.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := preview.Fetch(ctx, dev, f, 40, 20, proto)
	sparseElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != preview.TierVideo || len(res.Rendered) == 0 {
		t.Fatalf("no frame: tier=%v note=%q", res.Tier, res.Note)
	}
	t.Logf("sparse frame grab: %s", sparseElapsed.Round(time.Millisecond))

	// Full pull, for the comparison.
	full := filepath.Join(dir, "full.mp4")
	start = time.Now()
	if err := dev.Pull(ctx, f.Path, full, nil); err != nil {
		t.Fatalf("full pull: %v", err)
	}
	fullPullElapsed := time.Since(start)
	fullFrame, err := preview.GrabFrame(ctx, full)
	if err != nil {
		t.Fatalf("GrabFrame(full): %v", err)
	}
	t.Logf("full pull:         %s (%d MB)", fullPullElapsed.Round(time.Millisecond), f.Size>>20)
	t.Logf("speedup:           %.1fx", float64(fullPullElapsed)/float64(sparseElapsed))

	if sparseElapsed*10 > fullPullElapsed {
		t.Errorf("sparse (%s) is not meaningfully faster than a full pull (%s) — "+
			"re-measure before trusting the design",
			sparseElapsed.Round(time.Millisecond), fullPullElapsed.Round(time.Millisecond))
	}

	// The frame must be the same one. Re-render the full-pull frame through
	// the identical decode/downscale/render chain so the comparison is of the
	// bytes the pane would actually draw.
	sparseSum := sha256.Sum256(res.Rendered)
	fullRendered := renderJPEG(t, fullFrame, proto)
	if sparseSum != sha256.Sum256(fullRendered) {
		t.Error("the sparse reconstruction produced a different frame than the full pull")
	}
	_ = os.Remove(full)
}

// TestRealAudioPreviewAndProbe exercises the audio tier against a real file:
// pull, ffprobe, waveform, render.
func TestRealAudioPreviewAndProbe(t *testing.T) {
	dev := realDevice(t)
	if !preview.HasFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ix, _ := index.Load(ctx, dev)

	var pick device.File
	for _, f := range ix.All {
		if f.Kind() == device.KindAudio && f.Size > 50_000 {
			pick = f
			break
		}
	}
	if pick.Size == 0 {
		t.Skip("no audio on device")
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, ".cache"))

	start := time.Now()
	res, err := preview.Fetch(ctx, dev, pick, 40, 12, preview.ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", pick.Name, err)
	}
	t.Logf("%s (%d KB): tier=%v rendered=%dB local=%q in %s",
		pick.Name, pick.Size>>10, res.Tier, len(res.Rendered), res.Local,
		time.Since(start).Round(time.Millisecond))
	for _, l := range res.Meta {
		t.Logf("  %s", l)
	}
	if res.Tier != preview.TierAudio {
		t.Fatalf("tier = %v, want TierAudio (note %q)", res.Tier, res.Note)
	}
	if res.Local == "" {
		t.Error("audio preview must cache the pulled file for playback")
	}
	if res.Duration <= 0 {
		t.Error("no duration from ffprobe or MediaStore")
	}
}

func renderJPEG(t *testing.T, jpg []byte, proto preview.Protocol) []byte {
	t.Helper()
	out, err := preview.RenderImage(jpg, "image/jpeg", 40, 20, proto)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}
