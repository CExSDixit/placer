package preview

import (
	"fmt"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

// MetaLines is the metadata card: what placer can say about a file from the
// MediaStore row alone, with no bytes fetched. It is the universal fallback —
// what heic has shown since phase 2, what a video shows when autoplay is off,
// and what anything shows when ffmpeg is absent.
func MetaLines(f device.File) []string {
	var out []string
	add := func(k, v string) {
		if v != "" {
			out = append(out, k+"  "+v)
		}
	}
	add("size", humanBytes(f.Size))
	if f.Duration > 0 {
		add("length", humanDuration(f.Duration))
	}
	add("type", f.Mime)
	add("album", f.Bucket)
	if t := f.SortTime(); !t.IsZero() {
		add("date", t.Format("2006-01-02 15:04"))
	}
	if f.Duration > 0 && f.Size > 0 {
		add("bitrate", fmt.Sprintf("%.1f Mb/s", float64(f.Size)*8/f.Duration.Seconds()/1e6))
	}
	return out
}

func metaResult(f device.File, note string) Result {
	return Result{Tier: TierMeta, Note: note, Meta: MetaLines(f)}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// FormatPosition renders a playhead as "1:23 / 4:56".
func FormatPosition(pos, total time.Duration) string {
	if total <= 0 {
		return humanDuration(pos)
	}
	if pos > total {
		pos = total
	}
	return humanDuration(pos) + " / " + humanDuration(total)
}
