package device

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Fake is a Device backed by fixtures or synthetic data, so the TUI, indexer
// and transfer engine can be built and tested with no phone attached.
//
// It reproduces the measured behaviour of the real device: the library
// composition observed on the Pixel 6a, and ~23 MB/s transfers.
type Fake struct {
	serial  string
	rows    map[Collection][]map[string]string
	SrcDir  string        // if set, Pull copies real bytes from here
	Latency time.Duration // simulated per-command round trip
	Speed   float64       // simulated bytes/sec for Pull
	sizes   map[string]int64
}

// indexSizes lets Pull produce files of the size MediaStore advertises, so the
// transfer engine's size verification is exercised rather than bypassed.
func (f *Fake) indexSizes() {
	f.sizes = map[string]int64{}
	for _, rows := range f.rows {
		for _, r := range rows {
			if p := r["_data"]; p != "" {
				f.sizes[p] = atoi64(r["_size"])
			}
		}
	}
}

func (f *Fake) Serial() string { return f.serial }

func (f *Fake) Query(ctx context.Context, q Query) ([]map[string]string, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(f.Latency):
	}
	src := f.rows[q.Coll]
	out := make([]map[string]string, 0, len(src))
	for _, r := range src {
		row := make(map[string]string, len(q.Projection))
		for _, k := range q.Projection {
			if v, ok := r[k]; ok {
				row[k] = v // absent keys stay absent, as content query does
			}
		}
		out = append(out, row)
	}
	if strings.Contains(q.Sort, "DESC") {
		key := strings.Fields(q.Sort)[0]
		sort.SliceStable(out, func(i, j int) bool { return out[i][key] > out[j][key] })
	}
	return out, nil
}

func (f *Fake) ExecOut(ctx context.Context, cmd string) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(f.Latency):
	}
	if out, ok, err := f.fakeDD(cmd); ok {
		return out, err
	}
	return nil, fmt.Errorf("fake: ExecOut not implemented for %q", cmd)
}

// ddRe matches the `dd` invocations the sparse video preview issues, in any
// combination of skip/count:
//
//	dd if='<path>' bs=<n> count=<n> 2>/dev/null           the header
//	dd if='<path>' bs=<n> skip=<n> count=<n> 2>/dev/null  a scrub window
//	dd if='<path>' bs=<n> skip=<n> 2>/dev/null            the tail, to EOF
//
// Reproducing them against SrcDir is what lets the sparse reconstruction —
// the one place where an offset error silently yields a corrupt frame rather
// than an error — be tested with no phone attached.
var ddRe = regexp.MustCompile(`^dd if='(.*)' bs=(\d+)((?: (?:skip|count)=\d+)*) 2>/dev/null$`)

func (f *Fake) fakeDD(cmd string) ([]byte, bool, error) {
	m := ddRe.FindStringSubmatch(cmd)
	if m == nil {
		return nil, false, nil
	}
	if f.SrcDir == "" {
		return nil, true, fmt.Errorf("fake: dd needs SrcDir")
	}
	fh, err := os.Open(filepath.Join(f.SrcDir, filepath.Base(m[1])))
	if err != nil {
		return nil, true, err
	}
	defer fh.Close()

	bs, _ := strconv.ParseInt(m[2], 10, 64)
	var skip, count int64 = 0, -1
	for _, opt := range strings.Fields(m[3]) {
		k, v, _ := strings.Cut(opt, "=")
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "skip":
			skip = n
		case "count":
			count = n
		}
	}
	if skip > 0 {
		if _, err := fh.Seek(skip*bs, io.SeekStart); err != nil {
			return nil, true, err
		}
	}
	// `2>/dev/null` on the device is what keeps dd's "4+0 records in" summary
	// out of the payload — adb exec-out folds device stderr into stdout, and
	// real hardware appends exactly 78 bytes without it. The fake emits the
	// payload alone, matching the corrected command.
	if count >= 0 {
		return readFull(fh, count*bs)
	}
	b, err := io.ReadAll(fh)
	return b, true, err
}

func readFull(r io.Reader, n int64) ([]byte, bool, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil // a short read at EOF is exactly what dd does
	}
	return buf[:got], true, err
}

func (f *Fake) Pull(ctx context.Context, remote, local string, prog func(Progress)) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	size := f.sizes[remote]
	if size == 0 {
		size = 2_500_000
	}
	var data []byte
	if f.SrcDir != "" {
		b, err := os.ReadFile(filepath.Join(f.SrcDir, filepath.Base(remote)))
		if err == nil {
			data, size = b, int64(len(b))
		}
	}
	if size <= 0 {
		size = 1
	}
	total := time.Duration(float64(size) / f.Speed * float64(time.Second))
	steps := 10
	for i := 1; i <= steps; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(total / time.Duration(steps)):
		}
		if prog != nil {
			prog(Progress{Path: remote, Percent: i * 100 / steps})
		}
	}
	if data == nil {
		data = make([]byte, size)
	}
	return os.WriteFile(local, data, 0o644)
}

// LoadFixtures builds a Fake from recorded `content query` output. Files are
// named after the collection: images.txt, video.txt, audio.txt, downloads.txt,
// file.txt. Capture them with cmd/capture-fixtures against a real device.
func LoadFixtures(dir string) (*Fake, error) {
	f := &Fake{serial: "fixture", rows: map[Collection][]map[string]string{},
		Latency: 5 * time.Millisecond, Speed: 23 << 20}
	for name, coll := range map[string]Collection{
		"images": Images, "video": Video, "audio": Audio,
		"downloads": Downloads, "file": Files,
	} {
		b, err := os.ReadFile(filepath.Join(dir, name+".txt"))
		if err != nil {
			continue // a missing collection is fine
		}
		f.rows[coll] = ParseRows(string(b), StandardProjection(coll))
	}
	if len(f.rows) == 0 {
		return nil, fmt.Errorf("no fixtures found in %s", dir)
	}
	f.indexSizes()
	return f, nil
}

// mimes mirrors the distribution measured on the Pixel 6a on 2026-07-25, so
// development happens at realistic scale and composition.
var imageMimes = []struct {
	mime string
	n    int
	ext  string
}{
	{"image/jpeg", 7513, "jpg"},
	{"image/png", 2819, "png"},
	{"image/x-adobe-dng", 54, "dng"},
	{"image/heic", 2, "heic"},
	{"image/gif", 2, "gif"},
	{"image/heif", 1, "heif"},
	{"image/svg+xml", 1, "svg"},
}

var audioMimes = []struct {
	mime string
	n    int
	ext  string
}{
	{"audio/x-wav", 64, "wav"},
	{"audio/mpeg", 45, "mp3"},
	{"audio/ogg", 15, "ogg"},
}

// Adversarial names that exercise the parser and the renderer.
var trickyNames = []string{
	"Trip to Paris, France.jpg",
	"Song, The (Remix).mp3",
	"a=b, c.png",
	"  leading and trailing  .jpg",
	"emoji 📸 shot.jpg",
	"very_long_" + strings.Repeat("name_", 20) + "end.jpg",
}

// Synthetic builds a Fake with a realistically-shaped library. Deterministic
// for a given seed so tests and screenshots are reproducible.
func Synthetic(seed int64) *Fake {
	rng := rand.New(rand.NewSource(seed))
	f := &Fake{serial: "synthetic", rows: map[Collection][]map[string]string{},
		Latency: 5 * time.Millisecond, Speed: 23 << 20}

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	id := 0
	mk := func(coll Collection, dir, name, mime string, size int64, dur time.Duration) map[string]string {
		id++
		taken := base.Add(time.Duration(rng.Int63n(int64(600 * 24 * time.Hour))))
		r := map[string]string{
			"_id":                 fmt.Sprint(id),
			"_data":               dir + "/" + name,
			"_display_name":       name,
			"_size":               fmt.Sprint(size),
			"mime_type":           mime,
			"date_added":          fmt.Sprint(taken.Unix()),
			"bucket_display_name": filepath.Base(dir),
		}
		if coll == Images || coll == Video {
			r["datetaken"] = fmt.Sprint(taken.UnixMilli())
		}
		if dur > 0 {
			r["duration"] = fmt.Sprint(dur.Milliseconds())
		}
		return r
	}

	for _, m := range imageMimes {
		for i := 0; i < m.n; i++ {
			dir := "/storage/emulated/0/DCIM/Camera"
			if m.mime == "image/png" {
				dir = "/storage/emulated/0/Pictures/Screenshots"
			}
			name := fmt.Sprintf("PXL_2026%02d%02d_%06d.%s", 1+rng.Intn(12), 1+rng.Intn(28), rng.Intn(999999), m.ext)
			size := int64(1_500_000 + rng.Intn(4_000_000))
			f.rows[Images] = append(f.rows[Images], mk(Images, dir, name, m.mime, size, 0))
		}
	}
	for i := 0; i < 482; i++ {
		name := fmt.Sprintf("PXL_2026%02d%02d_%06d.mp4", 1+rng.Intn(12), 1+rng.Intn(28), rng.Intn(999999))
		size := int64(5_000_000 + rng.Int63n(1_800_000_000))
		dur := time.Duration(10+rng.Intn(600)) * time.Second
		f.rows[Video] = append(f.rows[Video], mk(Video, "/storage/emulated/0/DCIM/Camera", name, "video/mp4", size, dur))
	}
	f.rows[Video] = append(f.rows[Video], mk(Video, "/storage/emulated/0/DCIM/Camera", "clip.3gp", "video/3gpp", 400_000, 12*time.Second))

	for _, m := range audioMimes {
		for i := 0; i < m.n; i++ {
			name := fmt.Sprintf("Recording_%03d.%s", i+1, m.ext)
			size := int64(100_000 + rng.Intn(5_000_000))
			dur := time.Duration(5+rng.Intn(900)) * time.Second
			row := mk(Audio, "/storage/emulated/0/Recordings", name, m.mime, size, dur)
			// Audio frequently has no _display_name, only a title.
			if i%3 == 0 {
				delete(row, "_display_name")
				row["title"] = fmt.Sprintf("Voice %03d", i+1)
			}
			f.rows[Audio] = append(f.rows[Audio], row)
		}
	}

	for i, n := range trickyNames {
		coll := Images
		mime := "image/jpeg"
		if strings.HasSuffix(n, ".mp3") {
			coll, mime = Audio, "audio/mpeg"
		} else if strings.HasSuffix(n, ".png") {
			mime = "image/png"
		}
		f.rows[coll] = append(f.rows[coll], mk(coll, "/storage/emulated/0/Pictures", n, mime, int64(500_000+i), 0))
	}

	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("document_%02d.pdf", i)
		f.rows[Downloads] = append(f.rows[Downloads], mk(Downloads, "/storage/emulated/0/Download", name, "application/pdf", int64(80_000+rng.Intn(4_000_000)), 0))
	}
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("notes_%02d.md", i)
		f.rows[Downloads] = append(f.rows[Downloads], mk(Downloads, "/storage/emulated/0/Download", name, "text/markdown", int64(1_000+rng.Intn(40_000)), 0))
	}
	f.indexSizes()
	return f
}
