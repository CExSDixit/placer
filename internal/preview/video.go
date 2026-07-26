package preview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

// Sparse head+tail reconstruction.
//
// Camera MP4s put `moov` at the END of the file — measured, not assumed. The
// 894 MB reference recording lays out as:
//
//	ftyp at 0        (28 B)
//	mdat at 28       (893,938,585 B)
//	moov at 893,938,613 (532,585 B)
//
// so a naive frame grab has to pull the whole file: 42 s for the largest video
// on the reference device. Instead, fetch only the two regions that matter —
// the start of `mdat` (early frames) and the tail (the `moov` index) — into a
// locally sparse file whose logical size matches the real one. ffmpeg seeks
// via `moov` to a byte offset inside the head and never reads the hole.
//
// Two things here silently corrupt the frame rather than erroring, so both are
// guarded explicitly below:
//
//  1. The device's `dd` writes its "4+0 records in" summary to stderr, and
//     `adb exec-out` folds stderr into stdout — measured: exactly 78 extra
//     bytes appended to every payload. `2>/dev/null` runs ON THE DEVICE for
//     this reason. Without it the head is 4,194,382 bytes instead of
//     4,194,304 and the reconstruction is offset garbage.
//  2. WriteAt at the wrong offset produces a decodable-but-wrong file. The
//     tail's start offset plus its length must equal the file's true size;
//     anything else means the skip arithmetic is wrong and we bail rather
//     than render a corrupt frame.
const (
	mib = 1 << 20

	// sparseTail is sized to hold `moov`, which is 532 KB on the reference
	// 894 MB recording. Very long recordings carry a bigger index, hence the
	// retry tier before falling back to a full pull.
	sparseTail      = 8 * mib
	sparseTailRetry = 32 * mib

	// The head must cover enough of `mdat` to decode the frame at
	// frameSeek. It is derived from the real bitrate (MediaStore gives us
	// both `_size` and `duration`) rather than fixed: a fixed 4 MB head
	// covers 1.45 s at the reference recording's 2.63 MB/s, which lands the
	// decoder in the hole immediately after the wanted frame. The frame it
	// produced was still byte-identical to a full pull, but relying on
	// "the corruption starts after the bytes we needed" is not a design.
	sparseMinHead = 4 * mib
	sparseMaxHead = 24 * mib

	// frameSeek skips the first second: the opening frames of a camera
	// recording are routinely black or mid-autoexposure.
	frameSeek = 1 * time.Second

	// headSlack is how much video beyond frameSeek the head should cover, so
	// the decoder can read ahead past the wanted frame without hitting the
	// hole.
	headSlack = 2 * time.Second
)

var errSparseTooSmall = errors.New("preview: file too small for sparse reconstruction")

// headBytesFor sizes the head region from the file's real bitrate.
func headBytesFor(f device.File) int64 {
	want := int64(sparseMinHead)
	if f.Duration > 0 && f.Size > 0 {
		bps := float64(f.Size) / f.Duration.Seconds()
		want = int64(bps * (frameSeek + headSlack).Seconds())
	}
	return clamp64(want, sparseMinHead, sparseMaxHead)
}

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ddHead reads the first n bytes of remote. bs is a whole mebibyte and count
// is rounded up, so the payload is a multiple of 1 MiB and never a short read
// mid-block.
func ddHead(remote string, mibs int64) string {
	return fmt.Sprintf("dd if=%s bs=%d count=%d 2>/dev/null",
		device.ShellQuote(remote), mib, mibs)
}

// ddTail reads from a mebibyte-aligned offset to EOF. `skip` counts input
// blocks, so the byte offset is skip*mib exactly — this is the number the
// local WriteAt must agree with.
func ddTail(remote string, skipMiB int64) string {
	return fmt.Sprintf("dd if=%s bs=%d skip=%d 2>/dev/null",
		device.ShellQuote(remote), mib, skipMiB)
}

// buildSparse fetches the head and tail of f and reconstructs them into a
// local sparse file at dst whose logical size equals f.Size. Returns the
// number of bytes actually transferred, for the timing story.
func buildSparse(ctx context.Context, dev device.Device, f device.File, dst string, tailBytes int64) (int64, error) {
	size := f.Size
	headBytes := headBytesFor(f)
	headMiB := (headBytes + mib - 1) / mib
	headBytes = headMiB * mib

	// Align the tail down to a mebibyte so `dd skip=` addresses it exactly.
	tailStart := ((size - tailBytes) / mib) * mib
	if size <= 0 || tailStart <= headBytes {
		// The regions would overlap: the file is small enough that a full
		// pull is cheaper than this dance anyway.
		return 0, errSparseTooSmall
	}

	head, err := dev.ExecOut(ctx, ddHead(f.Path, headMiB))
	if err != nil {
		return 0, fmt.Errorf("sparse head: %w", err)
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	tail, err := dev.ExecOut(ctx, ddTail(f.Path, tailStart/mib))
	if err != nil {
		return 0, fmt.Errorf("sparse tail: %w", err)
	}

	// Guard 2. If the tail's true offset were anything other than tailStart,
	// these would not add up — and a misplaced WriteAt yields a corrupt frame
	// instead of an error, which is the whole reason this check exists.
	if got := tailStart + int64(len(tail)); got != size {
		return 0, fmt.Errorf("sparse tail lands at %d bytes, file is %d — refusing to reconstruct", got, size)
	}
	if int64(len(head)) > tailStart {
		return 0, fmt.Errorf("sparse head (%d B) overruns tail start (%d)", len(head), tailStart)
	}

	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	// Truncate first: everything between the head and the tail stays a hole
	// and is never read, written or transferred.
	if err := out.Truncate(size); err != nil {
		return 0, err
	}
	if _, err := out.WriteAt(head, 0); err != nil {
		return 0, err
	}
	if _, err := out.WriteAt(tail, tailStart); err != nil {
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return int64(len(head) + len(tail)), nil
}

// GrabFrame extracts a single still from a local video file as JPEG bytes.
// Seeks past the first second (opening frames are usually black), falling
// back to the very first frame for clips shorter than that.
func GrabFrame(ctx context.Context, src string) ([]byte, error) {
	bin := Tool("ffmpeg")
	if bin == "" {
		return nil, errors.New("ffmpeg not on PATH")
	}
	for _, ss := range []string{fmt.Sprintf("%.2f", frameSeek.Seconds()), "0"} {
		jpg := filepath.Join(filepath.Dir(src), "frame.jpg")
		_ = os.Remove(jpg)
		// -ss BEFORE -i is the input seek: ffmpeg uses the moov index to jump
		// straight to a keyframe rather than decoding from the start, which is
		// what keeps this off the sparse file's hole.
		cmd := exec.CommandContext(ctx, bin,
			"-v", "error", "-ss", ss, "-i", src,
			"-frames:v", "1", "-q:v", "2", "-y", jpg)
		runErr := cmd.Run()
		b, readErr := os.ReadFile(jpg)
		_ = os.Remove(jpg)
		if readErr == nil && len(b) > 0 {
			// ffmpeg exits non-zero on decode warnings while still writing a
			// good frame — the sparse read-ahead into the hole does exactly
			// that. A frame we could read is a frame we can show.
			return b, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_ = runErr
	}
	return nil, errors.New("ffmpeg produced no frame")
}

// buildAndGrab is one rung of the escalation ladder: reconstruct with the
// given tail size, then try to decode a frame out of it. On success it writes
// the JPEG through jpg.
func buildAndGrab(ctx context.Context, dev device.Device, f device.File, sparse string, tail int64, jpg *[]byte) error {
	if _, err := buildSparse(ctx, dev, f, sparse, tail); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b, err := GrabFrame(ctx, sparse)
	if err != nil {
		return err
	}
	*jpg = b
	return nil
}

// fetchVideo renders a still frame for a video, escalating only as far as it
// has to: sparse with an 8 MB tail, sparse with a 32 MB tail (a very long
// recording's `moov` can exceed 8 MB), then a full pull, then metadata only.
func fetchVideo(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	if !HasFFmpeg() {
		return metaResult(f, "video — ffmpeg not installed"), nil
	}
	if cached, ok := readCache(f, cellW, cellH, proto); ok {
		return Result{Tier: TierVideo, Rendered: cached, Meta: MetaLines(f)}, nil
	}

	dir, err := os.MkdirTemp("", "placer-video-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	sparse := filepath.Join(dir, "sparse.mp4")

	var jpg []byte
	for _, tail := range []int64{sparseTail, sparseTailRetry} {
		if err := buildAndGrab(ctx, dev, f, sparse, tail, &jpg); err != nil {
			if errors.Is(err, errSparseTooSmall) {
				break // straight to the full pull; it's a small file
			}
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			continue // a bigger tail may hold a `moov` the 8 MB one clipped
		}
		break
	}

	if jpg == nil {
		// Last resort: the whole file. Slow (42 s on the reference device's
		// largest video) but correct, and it only happens when the sparse
		// reconstruction genuinely could not be decoded.
		full := filepath.Join(dir, "full.mp4")
		if err := dev.Pull(ctx, f.Path, full, nil); err != nil {
			return Result{}, err
		}
		b, err := GrabFrame(ctx, full)
		if err != nil {
			return metaResult(f, "video — no frame: "+err.Error()), nil
		}
		jpg = b
	}

	rendered, err := RenderImage(jpg, "image/jpeg", cellW, cellH, proto)
	if err != nil {
		return metaResult(f, "video — frame render failed: "+err.Error()), nil
	}
	writeCache(f, cellW, cellH, proto, rendered)
	return Result{Tier: TierVideo, Rendered: rendered, Meta: MetaLines(f)}, nil
}
