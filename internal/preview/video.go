package preview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// DefaultFrameSeek is where a video preview grabs its frame before any
// scrubbing — one second in, past the black opening frames.
const DefaultFrameSeek = frameSeek

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

func alignDown(v int64) int64 { return (v / mib) * mib }
func alignUp(v int64) int64   { return ((v + mib - 1) / mib) * mib }

// region is one mebibyte-aligned span to fetch off the device.
type region struct{ off, length int64 }

// sparseRegions is what has to be fetched to decode a frame at `at`.
//
// Always three things, which may merge into fewer:
//
//  1. The header. `ftyp` is 28 bytes at offset 0 and ffmpeg's mov probe needs
//     it to identify the container at all, so offset 0 is never left a hole
//     no matter how deep the seek.
//  2. A window around the seek point, positioned from the real bitrate
//     (MediaStore gives both `_size` and `duration`). Backed off far enough
//     to include the keyframe *before* `at`, since that is what the decoder
//     actually starts from.
//  3. The tail holding `moov`.
func sparseRegions(f device.File, at time.Duration, tailBytes int64) []region {
	head := alignUp(headBytesFor(f))
	tailStart := alignDown(f.Size - tailBytes)
	regs := []region{{0, head}}

	if at > frameSeek && f.Duration > 0 && f.Size > 0 {
		bps := float64(f.Size) / f.Duration.Seconds()
		// Back off by three seconds of video (min 2 MiB) so the keyframe the
		// decoder rewinds to is inside the window, not in the hole.
		back := int64(max(2*mib, int(bps*3)))
		start := alignDown(int64(bps*at.Seconds()) - back)
		if start < 0 {
			start = 0
		}
		length := alignUp(int64(bps*6) + 4*mib)
		regs = append(regs, region{start, length})
	}

	regs = append(regs, region{tailStart, f.Size - tailStart})
	return normalizeRegions(regs, f.Size)
}

// normalizeRegions clamps to the file, sorts, and merges anything that
// overlaps or abuts — a seek early in a short clip lands inside the header,
// and fetching the same megabyte twice would be pure waste.
func normalizeRegions(in []region, size int64) []region {
	var rs []region
	for _, r := range in {
		if r.off < 0 {
			r.length += r.off
			r.off = 0
		}
		if r.off >= size || r.length <= 0 {
			continue
		}
		if r.off+r.length > size {
			r.length = size - r.off
		}
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].off < rs[j].off })

	var out []region
	for _, r := range rs {
		if n := len(out); n > 0 && r.off <= out[n-1].off+out[n-1].length {
			if end := r.off + r.length; end > out[n-1].off+out[n-1].length {
				out[n-1].length = end - out[n-1].off
			}
			continue
		}
		out = append(out, r)
	}
	return out
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

// ddRange reads countMiB blocks starting at skipMiB — the window around a
// seek point, for scrubbing to a frame deeper than the head covers.
func ddRange(remote string, skipMiB, countMiB int64) string {
	return fmt.Sprintf("dd if=%s bs=%d skip=%d count=%d 2>/dev/null",
		device.ShellQuote(remote), mib, skipMiB, countMiB)
}

// buildSparse fetches the regions needed to decode a frame at `at` and
// reconstructs them into a local sparse file at dst whose logical size equals
// f.Size. Returns the number of bytes actually transferred.
func buildSparse(ctx context.Context, dev device.Device, f device.File, dst string, at time.Duration, tailBytes int64) (int64, error) {
	size := f.Size
	regs := sparseRegions(f, at, tailBytes)
	if size <= 0 || len(regs) < 2 {
		// Everything merged into one span: the file is small enough that a
		// full pull is cheaper than this dance anyway.
		return 0, errSparseTooSmall
	}
	last := regs[len(regs)-1]
	if last.off+last.length != size {
		return 0, fmt.Errorf("sparse regions end at %d, file is %d", last.off+last.length, size)
	}

	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	// A reconstruction that is rejected part-way must not leave a plausible
	// file behind for ffmpeg to decode into a wrong frame.
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	// Truncate first: everything between the regions stays a hole and is
	// never read, written or transferred.
	if err := out.Truncate(size); err != nil {
		return 0, err
	}

	var moved int64
	for i, r := range regs {
		var cmd string
		if r.off+r.length >= size {
			cmd = ddTail(f.Path, r.off/mib) // to EOF
		} else {
			cmd = ddRange(f.Path, r.off/mib, r.length/mib)
		}
		b, err := dev.ExecOut(ctx, cmd)
		if err != nil {
			return 0, fmt.Errorf("sparse region %d: %w", i, err)
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}

		// The guard that matters. A WriteAt at the wrong offset yields a
		// decodable-but-wrong file rather than an error, so the final region
		// must land exactly on the end of the file; anything else means the
		// skip arithmetic is wrong and we refuse rather than render a lie.
		if i == len(regs)-1 {
			if got := r.off + int64(len(b)); got != size {
				return 0, fmt.Errorf("sparse tail lands at %d bytes, file is %d — refusing to reconstruct", got, size)
			}
		} else if int64(len(b)) != r.length {
			return 0, fmt.Errorf("sparse region %d: got %d bytes, wanted %d", i, len(b), r.length)
		}

		if _, err := out.WriteAt(b, r.off); err != nil {
			return 0, err
		}
		moved += int64(len(b))
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	ok = true
	return moved, nil
}

// GrabFrame extracts a single still from a local video file as JPEG bytes,
// at the default seek point.
func GrabFrame(ctx context.Context, src string) ([]byte, error) {
	return GrabFrameAt(ctx, src, frameSeek)
}

// GrabFrameAt extracts a still at `at`, falling back to the very first frame
// when the seek lands past the end of a short clip.
func GrabFrameAt(ctx context.Context, src string, at time.Duration) ([]byte, error) {
	bin := Tool("ffmpeg")
	if bin == "" {
		return nil, errors.New("ffmpeg not on PATH")
	}
	for _, ss := range []string{fmt.Sprintf("%.2f", at.Seconds()), "0"} {
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
func buildAndGrab(ctx context.Context, dev device.Device, f device.File, sparse string, at time.Duration, tail int64, jpg *[]byte) error {
	if _, err := buildSparse(ctx, dev, f, sparse, at, tail); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b, err := GrabFrameAt(ctx, sparse, at)
	if err != nil {
		return err
	}
	*jpg = b
	return nil
}

// frameVariant is the cache-key suffix for a scrubbed frame, so a frame at
// 0:30 never serves as the frame at 1:00.
func frameVariant(at time.Duration) string {
	return fmt.Sprintf("f%d", at.Milliseconds())
}

// fetchVideo renders a still frame for a video at `at`, escalating only as
// far as it has to: sparse with an 8 MB tail, sparse with a 32 MB tail (a
// very long recording's `moov` can exceed 8 MB), then a full pull, then
// metadata only.
func fetchVideo(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol, at time.Duration) (Result, error) {
	if !HasFFmpeg() {
		return metaResult(f, "video — ffmpeg not installed"), nil
	}
	if f.Duration > 0 && at >= f.Duration {
		at = f.Duration - time.Second
	}
	if at < 0 {
		at = 0
	}
	meta := append(MetaLines(f), "frame  "+humanDuration(at))
	variant := frameVariant(at)
	if cached, ok := readCache(f, cellW, cellH, proto, variant); ok {
		return Result{Tier: TierVideo, Rendered: cached, Meta: meta, At: at}, nil
	}

	dir, err := os.MkdirTemp("", "placer-video-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	sparse := filepath.Join(dir, "sparse.mp4")

	var jpg []byte
	for _, tail := range []int64{sparseTail, sparseTailRetry} {
		if err := buildAndGrab(ctx, dev, f, sparse, at, tail, &jpg); err != nil {
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
		b, err := GrabFrameAt(ctx, full, at)
		if err != nil {
			return metaResult(f, "video — no frame: "+err.Error()), nil
		}
		jpg = b
	}

	rendered, err := RenderImage(jpg, "image/jpeg", cellW, cellH, proto)
	if err != nil {
		return metaResult(f, "video — frame render failed: "+err.Error()), nil
	}
	writeCache(f, cellW, cellH, proto, variant, rendered)
	return Result{Tier: TierVideo, Rendered: rendered, Meta: meta, At: at}, nil
}
