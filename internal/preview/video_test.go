package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

// largeSample is the one file in testdata/sample big enough to exercise the
// sparse path meaningfully — a real 894 MB camera recording with `moov` at
// the end. Gitignored like everything else there; see
// rabbitholes/placer-phase3-handoff.md for how to re-pull it.
const largeSample = "PXL_20260613_234419633.TS.mp4"

func largeVideo(t *testing.T) (string, device.File) {
	t.Helper()
	dir := sampleDir(t)
	fi, err := os.Stat(filepath.Join(dir, largeSample))
	if err != nil {
		t.Skipf("large sample video not present: %v", err)
	}
	return dir, device.File{
		Path:     largeSample,
		Name:     largeSample,
		Size:     fi.Size(),
		Mime:     "video/mp4",
		Duration: 324 * time.Second,
		Coll:     device.Video,
	}
}

// TestBuildSparse_Offsets is the guard on the failure this whole design can
// produce silently: a WriteAt at the wrong offset yields a decodable file
// holding the wrong bytes, not an error. It checks the reconstruction against
// the source at both region boundaries.
func TestBuildSparse_Offsets(t *testing.T) {
	dir, f := largeVideo(t)
	dev := &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}

	dst := filepath.Join(t.TempDir(), "sparse.mp4")
	moved, err := buildSparse(context.Background(), dev, f, dst, DefaultFrameSeek, sparseTail)
	if err != nil {
		t.Fatalf("buildSparse: %v", err)
	}

	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != f.Size {
		t.Fatalf("sparse logical size = %d, want %d", st.Size(), f.Size)
	}
	if moved >= f.Size/4 {
		t.Fatalf("transferred %d bytes of a %d byte file — that is not sparse", moved, f.Size)
	}

	src, err := os.Open(filepath.Join(dir, largeSample))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	got, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()

	headBytes := ((headBytesFor(f) + mib - 1) / mib) * mib
	tailStart := ((f.Size - sparseTail) / mib) * mib

	// The head, the last MiB before the hole starts, and the first and last
	// MiB of the tail must all be byte-identical to the source at the same
	// offsets. An off-by-one-block skip shows up in the tail checks.
	for _, region := range []struct {
		name string
		off  int64
		n    int64
	}{
		{"head start", 0, mib},
		{"head end", headBytes - mib, mib},
		{"tail start", tailStart, mib},
		{"tail end", f.Size - mib, mib},
	} {
		a := readAt(t, src, region.off, region.n)
		b := readAt(t, got, region.off, region.n)
		if !bytes.Equal(a, b) {
			t.Errorf("%s (offset %d): reconstruction differs from source", region.name, region.off)
		}
	}

	// The hole must genuinely be a hole, not fetched zeros that happened to
	// match — nothing between the regions was ever read off the device.
	mid := readAt(t, got, (headBytes+tailStart)/2, 4096)
	if !bytes.Equal(mid, make([]byte, 4096)) {
		t.Error("the gap between head and tail is not zero-filled")
	}
}

// TestGrabFrame_SparseMatchesFullPull is the claim the whole phase rests on:
// the frame from a 13 MB sparse reconstruction is the same frame a 894 MB
// full pull produces.
func TestGrabFrame_SparseMatchesFullPull(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	dir, f := largeVideo(t)
	dev := &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}

	dst := filepath.Join(t.TempDir(), "sparse.mp4")
	if _, err := buildSparse(context.Background(), dev, f, dst, DefaultFrameSeek, sparseTail); err != nil {
		t.Fatalf("buildSparse: %v", err)
	}

	sparseFrame, err := GrabFrame(context.Background(), dst)
	if err != nil {
		t.Fatalf("GrabFrame(sparse): %v", err)
	}
	fullFrame, err := GrabFrame(context.Background(), filepath.Join(dir, largeSample))
	if err != nil {
		t.Fatalf("GrabFrame(full): %v", err)
	}
	if sha256.Sum256(sparseFrame) != sha256.Sum256(fullFrame) {
		t.Errorf("sparse frame (%d B) differs from full-pull frame (%d B)",
			len(sparseFrame), len(fullFrame))
	}
}

// shortTailDevice returns a tail one block shorter than it should be — what a
// wrong `skip` would look like from the caller's side.
type shortTailDevice struct {
	*device.Fake
}

func (d shortTailDevice) ExecOut(ctx context.Context, cmd string) ([]byte, error) {
	b, err := d.Fake.ExecOut(ctx, cmd)
	if err != nil || len(b) <= mib {
		return b, err
	}
	if bytes.Contains([]byte(cmd), []byte("skip=")) {
		return b[mib:], nil
	}
	return b, nil
}

func TestBuildSparse_RejectsMisalignedTail(t *testing.T) {
	dir, f := largeVideo(t)
	dev := shortTailDevice{&device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}}

	dst := filepath.Join(t.TempDir(), "sparse.mp4")
	_, err := buildSparse(context.Background(), dev, f, dst, DefaultFrameSeek, sparseTail)
	if err == nil {
		t.Fatal("expected an error when the tail does not reach the end of the file")
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		if b, _ := os.ReadFile(dst); len(b) > 0 {
			t.Error("a rejected reconstruction must not leave a usable file behind")
		}
	}
}

func TestBuildSparse_TooSmallFallsBack(t *testing.T) {
	dir := sampleDir(t)
	small := sampleFile(t, dir, "VID-20260602-WA0003.mp4", "video/mp4")
	dev := &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}

	_, err := buildSparse(context.Background(), dev, small,
		filepath.Join(t.TempDir(), "sparse.mp4"), DefaultFrameSeek, sparseTail)
	if !errors.Is(err, errSparseTooSmall) {
		t.Fatalf("err = %v, want errSparseTooSmall", err)
	}
}

// TestFetchVideo_SmallFileFullPull covers the bottom of the escalation
// ladder: a file too small to reconstruct sparsely still gets a frame, via
// the full pull.
func TestFetchVideo_SmallFileFullPull(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "VID-20260602-WA0003.mp4", "video/mp4")
	f.Coll = device.Video

	res, err := Fetch(context.Background(), dev, f, 40, 12, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierVideo || len(res.Rendered) == 0 {
		t.Fatalf("got tier=%v rendered=%d note=%q", res.Tier, len(res.Rendered), res.Note)
	}
	if len(res.Meta) == 0 {
		t.Error("video preview should carry a metadata card alongside the frame")
	}
}

func TestHeadBytesFor_ScalesWithBitrate(t *testing.T) {
	// The reference recording: 894 MB over 324 s is 2.63 MB/s, and a fixed
	// 4 MB head covers only 1.45 s — the decoder lands in the hole right
	// after the frame we wanted. The head must cover frameSeek plus slack.
	f := device.File{Size: 894471198, Duration: 324 * time.Second}
	got := headBytesFor(f)
	want := int64(float64(f.Size) / f.Duration.Seconds() * (frameSeek + headSlack).Seconds())
	if got != want {
		t.Errorf("headBytesFor = %d, want %d", got, want)
	}
	if got <= sparseMinHead {
		t.Errorf("head %d did not scale above the %d floor for a 2.6 MB/s stream", got, sparseMinHead)
	}

	// No duration (MediaStore's column is routinely NULL) falls back to the
	// floor rather than dividing by zero.
	if got := headBytesFor(device.File{Size: 500 << 20}); got != sparseMinHead {
		t.Errorf("headBytesFor(no duration) = %d, want %d", got, sparseMinHead)
	}
	// A tiny, very-high-bitrate clip must not ask for the whole file.
	if got := headBytesFor(device.File{Size: 4 << 30, Duration: time.Second}); got != sparseMaxHead {
		t.Errorf("headBytesFor(huge bitrate) = %d, want the %d cap", got, sparseMaxHead)
	}
}

func TestDDCommandsRedirectDeviceStderr(t *testing.T) {
	// Measured on real hardware: without `2>/dev/null` running ON THE DEVICE,
	// adb exec-out folds dd's "4+0 records in" summary into stdout and every
	// payload arrives 78 bytes too long. Losing this suffix corrupts every
	// reconstruction silently, so it is asserted rather than assumed.
	for _, cmd := range []string{ddHead("/sdcard/a.mp4", 4), ddTail("/sdcard/a.mp4", 852)} {
		if !bytes.HasSuffix([]byte(cmd), []byte(" 2>/dev/null")) {
			t.Errorf("%q does not redirect device-side stderr", cmd)
		}
	}
	// A filename with an apostrophe must not break out of the quoting.
	if got := ddHead("/sdcard/it's here.mp4", 1); !bytes.Contains([]byte(got), []byte(`'/sdcard/it'\''s here.mp4'`)) {
		t.Errorf("apostrophe not escaped: %q", got)
	}
}

func readAt(t *testing.T, f *os.File, off, n int64) []byte {
	t.Helper()
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && int64(got) != n {
		t.Fatalf("ReadAt(%d, %d): %v", off, n, err)
	}
	return buf[:got]
}
