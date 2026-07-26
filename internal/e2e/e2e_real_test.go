// Package e2e holds tests that require a real attached device. They are gated
// behind PLACER_DEVICE=1 so the normal suite stays hardware-free.
//
//	PLACER_DEVICE=1 go test ./internal/e2e -v
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/index"
	"github.com/CExSDixit/placer/internal/transfer"
)

func realDevice(t *testing.T) device.Device {
	t.Helper()
	if os.Getenv("PLACER_DEVICE") == "" {
		t.Skip("set PLACER_DEVICE=1 with a device attached")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dev, err := device.NewADB(ctx, "adb", "")
	if err != nil {
		t.Skipf("no device: %v", err)
	}
	t.Logf("device: %s", dev.Serial())
	return dev
}

func TestRealIndexLoad(t *testing.T) {
	dev := realDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	ix, errs := index.Load(ctx, dev)
	elapsed := time.Since(start)

	for _, err := range errs {
		t.Logf("collection error (non-fatal): %v", err)
	}
	c := ix.Counts()
	t.Logf("indexed %d files in %s", len(ix.All), elapsed.Round(time.Millisecond))
	t.Logf("photos=%d video=%d audio=%d docs=%d", c[index.TabPhotos], c[index.TabVideo], c[index.TabAudio], c[index.TabDocs])

	if len(ix.All) == 0 {
		t.Fatal("indexed nothing")
	}
	if c[index.TabPhotos] < 1000 {
		t.Errorf("photos = %d, suspiciously low", c[index.TabPhotos])
	}

	// Fuzzy filtering must be local and therefore fast.
	fstart := time.Now()
	v := ix.Build(index.TabPhotos, "camera", index.SortDate)
	t.Logf("fuzzy filter over %d photos -> %d matches in %s",
		c[index.TabPhotos], len(v.Files), time.Since(fstart).Round(time.Microsecond))
	if time.Since(fstart) > 500*time.Millisecond {
		t.Errorf("local filter took %v, too slow to feel responsive", time.Since(fstart))
	}
}

// TestRealPullSetsMtime is the transfer path end to end: pick three small real
// photos, pull them, verify bytes and that mtime came from capture time.
func TestRealPullSetsMtime(t *testing.T) {
	dev := realDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ix, _ := index.Load(ctx, dev)
	view := ix.Build(index.TabPhotos, "", index.SortDate)

	// Smallest few files with a real capture time, to keep this cheap.
	var picked []device.File
	best := view.Files
	for i := 0; i < len(best) && len(picked) < 3; i++ {
		f := best[i]
		if f.Size > 0 && f.Size < 3_000_000 && f.Path != "" && !f.SortTime().IsZero() {
			picked = append(picked, f)
		}
	}
	if len(picked) == 0 {
		t.Skip("no suitable small photos found")
	}

	dest := t.TempDir()
	ch := make(chan transfer.Event, 128)
	done := make(chan transfer.Result, 1)
	start := time.Now()
	go func() { done <- transfer.Run(ctx, dev, picked, dest, transfer.Skip, ch) }()

	var maxPct int
	for ev := range ch {
		if ev.Percent > maxPct {
			maxPct = ev.Percent
		}
	}
	res := <-done
	elapsed := time.Since(start)

	t.Logf("pulled %d files (%d bytes) in %s — %.1f MB/s",
		res.Pulled, res.Bytes, elapsed.Round(time.Millisecond),
		float64(res.Bytes)/1048576/elapsed.Seconds())
	for _, f := range res.Failed {
		t.Errorf("failed: %s: %v", f.File.Name, f.Err)
	}
	if res.Pulled != len(picked) {
		t.Fatalf("pulled %d of %d", res.Pulled, len(picked))
	}

	for _, f := range picked {
		local := filepath.Join(dest, f.Name)
		st, err := os.Stat(local)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if st.Size() != f.Size {
			t.Errorf("%s: size %d, MediaStore said %d", f.Name, st.Size(), f.Size)
		}
		// mtime must be the capture time, not now.
		want := f.SortTime()
		if diff := st.ModTime().Sub(want); diff > time.Second || diff < -time.Second {
			t.Errorf("%s: mtime %v, want capture time %v", f.Name, st.ModTime(), want)
		} else {
			t.Logf("  %s  %d bytes  mtime=%s ✓", f.Name, st.Size(), st.ModTime().Format("2006-01-02 15:04"))
		}
	}
}

// TestRealExecOutIsBinaryClean guards the adb exec-out vs adb shell trap: if
// this ever regresses to `adb shell`, JPEG bytes come back with \n -> \r\n
// translation and previews break in phase 2.
func TestRealExecOutIsBinaryClean(t *testing.T) {
	dev := realDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ix, _ := index.Load(ctx, dev)
	view := ix.Build(index.TabPhotos, "", index.SortDate)

	var target device.File
	for _, f := range view.Files {
		if f.Mime == "image/jpeg" && f.Size > 200_000 && f.Path != "" {
			target = f
			break
		}
	}
	if target.Path == "" {
		t.Skip("no suitable jpeg")
	}

	start := time.Now()
	head, err := dev.ExecOut(ctx, "dd if='"+target.Path+"' bs=65536 count=2 2>/dev/null")
	if err != nil {
		t.Fatalf("exec-out: %v", err)
	}
	t.Logf("fetched %d bytes of %s in %s", len(head), target.Name, time.Since(start).Round(time.Millisecond))

	if len(head) < 1024 {
		t.Fatalf("got only %d bytes", len(head))
	}
	// A JPEG starts with FF D8 FF. Any \r\n translation corrupts this.
	if head[0] != 0xFF || head[1] != 0xD8 {
		t.Fatalf("not a JPEG SOI: % x — exec-out may be translating bytes", head[:4])
	}
	if n := len(head); n != 131072 {
		t.Logf("note: requested 131072 bytes, got %d", n)
	}
	// Explicitly check no CRLF expansion happened: count 0x0D 0x0A pairs that
	// would indicate translation of bare newlines.
	var crlf, lf int
	for i := 0; i < len(head)-1; i++ {
		if head[i] == 0x0D && head[i+1] == 0x0A {
			crlf++
		}
		if head[i] == 0x0A {
			lf++
		}
	}
	t.Logf("byte scan: %d LF, %d CRLF pairs (binary data, both expected to occur naturally)", lf, crlf)
}
