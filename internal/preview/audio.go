package preview

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

// MediaCacheDir holds whole media files pulled off the device, as opposed to
// the rendered thumbnails in CacheDir(). Audio needs the real bytes twice —
// once for the waveform, again for playback — and a voice memo the cursor
// rests on is a voice memo the user is about to press space on, so it is
// worth keeping rather than pulling twice.
func MediaCacheDir() string {
	return filepath.Join(cacheRoot(), "media")
}

// EnsureLocal pulls f to the media cache if it isn't already there and
// returns the local path. Pulls land on a temp name and are renamed into
// place, so a cancelled fetch can never leave a truncated file that a later
// call would happily hand to ffplay.
func EnsureLocal(ctx context.Context, dev device.Device, f device.File) (string, error) {
	ext := filepath.Ext(f.Name)
	dst := filepath.Join(MediaCacheDir(), cacheKey(f, 0, 0, ProtoHalfBlock)+ext)
	if st, err := os.Stat(dst); err == nil && (f.Size <= 0 || st.Size() == f.Size) {
		return dst, nil
	}
	if err := os.MkdirAll(MediaCacheDir(), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".partial"
	defer os.Remove(tmp)
	if err := dev.Pull(ctx, f.Path, tmp, nil); err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Probe is what ffprobe can tell us about a container that MediaStore can't.
type Probe struct {
	Duration time.Duration
	Codec    string
	Sample   string // sample rate, Hz
	Channels string
	BitRate  string
}

// ProbeFile reads container metadata. Returns a zero Probe and no error when
// ffprobe is absent — callers fall back to the MediaStore row.
func ProbeFile(ctx context.Context, path string) (Probe, error) {
	bin := Tool("ffprobe")
	if bin == "" {
		return Probe{}, nil
	}
	cmd := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "format=duration:stream=codec_name,sample_rate,channels,bit_rate",
		"-of", "default=noprint_wrappers=1",
		path)
	out, err := cmd.Output()
	if err != nil {
		return Probe{}, err
	}
	var p Probe
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok || v == "" || v == "N/A" {
			continue
		}
		switch strings.TrimSpace(k) {
		case "duration":
			if s, err := strconv.ParseFloat(v, 64); err == nil {
				p.Duration = time.Duration(s * float64(time.Second))
			}
		case "codec_name":
			p.Codec = v
		case "sample_rate":
			p.Sample = v
		case "channels":
			p.Channels = v
		case "bit_rate":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				p.BitRate = fmt.Sprintf("%.0f kb/s", float64(n)/1000)
			}
		}
	}
	return p, nil
}

// waveformPixels sizes the showwavespic render to the pane, in pixels, using
// the same ~2-pixel-rows-per-character-cell assumption as image previews.
func waveformPixels(cellW, cellH int) (int, int) {
	w := min(max(cellW*8, 160), 1200)
	h := min(max(cellH, 40), 400)
	if h%2 != 0 {
		h++
	}
	return w, h
}

// Waveform renders a PNG waveform overview of a local audio file.
func Waveform(ctx context.Context, src string, cellW, cellH int) ([]byte, error) {
	bin := Tool("ffmpeg")
	if bin == "" {
		return nil, fmt.Errorf("ffmpeg not on PATH")
	}
	dir, err := os.MkdirTemp("", "placer-wave-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "wave.png")

	pxW, pxH := waveformPixels(cellW, cellH)
	cmd := exec.CommandContext(ctx, bin,
		"-v", "error", "-i", src,
		"-filter_complex", fmt.Sprintf("showwavespic=s=%dx%d:colors=#7aa2f7", pxW, pxH),
		"-frames:v", "1", "-y", out)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("showwavespic: %w", err)
	}
	return os.ReadFile(out)
}

// fetchAudio pulls the file (cached for playback), probes it, and renders a
// waveform through the same decode/downscale/render chain as every image.
func fetchAudio(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	if !HasFFmpeg() && !HasFFprobe() {
		return metaResult(f, "audio — ffmpeg not installed"), nil
	}

	local, err := EnsureLocal(ctx, dev, f)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	meta := MetaLines(f)
	probe, perr := ProbeFile(ctx, local)
	if perr == nil {
		meta = append(meta, probeLines(f, probe)...)
	}

	res := Result{Tier: TierAudio, Meta: meta, Local: local, Duration: f.Duration}
	if probe.Duration > 0 {
		res.Duration = probe.Duration
	}

	if cached, ok := readCache(f, cellW, cellH, proto); ok {
		res.Rendered = cached
		return res, nil
	}
	png, err := Waveform(ctx, local, cellW, cellH)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		res.Note = "audio — no waveform"
		return res, nil
	}
	rendered, err := RenderImage(png, "image/png", cellW, cellH, proto)
	if err != nil {
		res.Note = "audio — waveform render failed"
		return res, nil
	}
	writeCache(f, cellW, cellH, proto, rendered)
	res.Rendered = rendered
	return res, nil
}

// probeLines reports what ffprobe adds beyond the MediaStore row, and flags a
// duration disagreement rather than silently preferring one: MediaStore's
// `duration` column is written by whatever app produced the file, while
// ffprobe reads the container itself, and they do not always match.
func probeLines(f device.File, p Probe) []string {
	var out []string
	if p.Codec != "" {
		out = append(out, "codec  "+p.Codec)
	}
	if p.Sample != "" {
		ch := p.Channels
		switch ch {
		case "1":
			ch = "mono"
		case "2":
			ch = "stereo"
		}
		out = append(out, "audio  "+p.Sample+" Hz "+ch)
	}
	if p.BitRate != "" {
		out = append(out, "rate   "+p.BitRate)
	}
	if p.Duration > 0 {
		if f.Duration > 0 && absDuration(p.Duration-f.Duration) > time.Second {
			out = append(out, fmt.Sprintf("probe  %s (MediaStore says %s)",
				humanDuration(p.Duration), humanDuration(f.Duration)))
		} else if f.Duration <= 0 {
			out = append(out, "length "+humanDuration(p.Duration))
		}
	}
	return out
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
