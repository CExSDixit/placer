package transfer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mishrasidhant/adbfz/internal/device"
)

// stub is a minimal Device: it writes exactly the bytes it is told to, so the
// transfer engine's size verification and mtime handling are exercised for
// real without a phone or multi-megabyte writes.
type stub struct {
	sizes map[string]int64
	fail  map[string]bool
	delay time.Duration
	pulls atomic.Int64 // workers run concurrently
}

func (s *stub) Serial() string { return "stub" }
func (s *stub) Query(context.Context, device.Query) ([]map[string]string, error) {
	return nil, nil
}
func (s *stub) ExecOut(context.Context, string) ([]byte, error) { return nil, nil }

func (s *stub) Pull(ctx context.Context, remote, local string, prog func(device.Progress)) error {
	s.pulls.Add(1)
	if s.fail[remote] {
		return fmt.Errorf("simulated failure")
	}
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.delay):
		}
	}
	if prog != nil {
		prog(device.Progress{Path: remote, Percent: 50})
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	return os.WriteFile(local, make([]byte, s.sizes[remote]), 0o644)
}

func run(t *testing.T, dev device.Device, files []device.File, dest string, pol Policy) Result {
	t.Helper()
	ch := make(chan Event, 256)
	done := make(chan Result, 1)
	go func() { done <- Run(context.Background(), dev, files, dest, pol, ch) }()
	for range ch {
	}
	return <-done
}

func file(name string, size int64, taken time.Time) device.File {
	return device.File{
		Path:  "/sdcard/DCIM/Camera/" + name,
		Name:  name,
		Size:  size,
		Mime:  "image/jpeg",
		Taken: taken,
		Coll:  device.Images,
	}
}

func TestPullsFilesAndSetsMtime(t *testing.T) {
	dest := t.TempDir()
	taken := time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)
	files := []device.File{
		file("a.jpg", 100, taken),
		file("b.jpg", 200, taken.Add(time.Hour)),
		file("c.jpg", 300, taken.Add(2*time.Hour)),
	}
	dev := &stub{sizes: map[string]int64{
		"/sdcard/DCIM/Camera/a.jpg": 100,
		"/sdcard/DCIM/Camera/b.jpg": 200,
		"/sdcard/DCIM/Camera/c.jpg": 300,
	}}

	res := run(t, dev, files, dest, Skip)
	if res.Pulled != 3 || len(res.Failed) != 0 {
		t.Fatalf("pulled=%d failed=%d", res.Pulled, len(res.Failed))
	}
	if res.Bytes != 600 {
		t.Errorf("bytes = %d, want 600", res.Bytes)
	}

	for _, f := range files {
		st, err := os.Stat(filepath.Join(dest, f.Name))
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if st.Size() != f.Size {
			t.Errorf("%s size = %d, want %d", f.Name, st.Size(), f.Size)
		}
		// mtime must come from capture time so Finder sorts correctly.
		if !st.ModTime().UTC().Equal(f.Taken.UTC()) {
			t.Errorf("%s mtime = %v, want %v", f.Name, st.ModTime().UTC(), f.Taken.UTC())
		}
	}
}

func TestSizeMismatchIsCaught(t *testing.T) {
	dest := t.TempDir()
	files := []device.File{file("a.jpg", 999, time.Now())}
	// Device delivers fewer bytes than MediaStore advertised.
	dev := &stub{sizes: map[string]int64{"/sdcard/DCIM/Camera/a.jpg": 10}}

	res := run(t, dev, files, dest, Skip)
	if res.Pulled != 0 || len(res.Failed) != 1 {
		t.Fatalf("pulled=%d failed=%d, want 0/1", res.Pulled, len(res.Failed))
	}
	if res.Failed[0].Err == nil {
		t.Fatal("expected a size mismatch error")
	}
}

func TestCollisionPolicies(t *testing.T) {
	taken := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := file("a.jpg", 100, taken)
	dev := &stub{sizes: map[string]int64{"/sdcard/DCIM/Camera/a.jpg": 100}}

	t.Run("skip", func(t *testing.T) {
		dest := t.TempDir()
		existing := filepath.Join(dest, "a.jpg")
		os.WriteFile(existing, []byte("original"), 0o644)

		res := run(t, dev, []device.File{f}, dest, Skip)
		if res.Skipped != 1 || res.Pulled != 0 {
			t.Fatalf("skipped=%d pulled=%d", res.Skipped, res.Pulled)
		}
		b, _ := os.ReadFile(existing)
		if string(b) != "original" {
			t.Error("skip overwrote the existing file")
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		dest := t.TempDir()
		existing := filepath.Join(dest, "a.jpg")
		os.WriteFile(existing, []byte("original"), 0o644)

		res := run(t, dev, []device.File{f}, dest, Overwrite)
		if res.Pulled != 1 {
			t.Fatalf("pulled = %d", res.Pulled)
		}
		st, _ := os.Stat(existing)
		if st.Size() != 100 {
			t.Errorf("not overwritten: size %d", st.Size())
		}
	})

	t.Run("rename", func(t *testing.T) {
		dest := t.TempDir()
		existing := filepath.Join(dest, "a.jpg")
		os.WriteFile(existing, []byte("original"), 0o644)

		res := run(t, dev, []device.File{f}, dest, Rename)
		if res.Pulled != 1 {
			t.Fatalf("pulled = %d", res.Pulled)
		}
		if b, _ := os.ReadFile(existing); string(b) != "original" {
			t.Error("rename clobbered the original")
		}
		if _, err := os.Stat(filepath.Join(dest, "a (1).jpg")); err != nil {
			t.Errorf("expected a (1).jpg: %v", err)
		}
	})
}

func TestFailuresAreReportedNotFatal(t *testing.T) {
	dest := t.TempDir()
	files := []device.File{
		file("ok1.jpg", 10, time.Now()),
		file("bad.jpg", 10, time.Now()),
		file("ok2.jpg", 10, time.Now()),
	}
	dev := &stub{
		sizes: map[string]int64{
			"/sdcard/DCIM/Camera/ok1.jpg": 10,
			"/sdcard/DCIM/Camera/bad.jpg": 10,
			"/sdcard/DCIM/Camera/ok2.jpg": 10,
		},
		fail: map[string]bool{"/sdcard/DCIM/Camera/bad.jpg": true},
	}

	res := run(t, dev, files, dest, Skip)
	if res.Pulled != 2 {
		t.Errorf("pulled = %d, want 2", res.Pulled)
	}
	if len(res.Failed) != 1 || res.Failed[0].File.Name != "bad.jpg" {
		t.Errorf("failures = %+v", res.Failed)
	}
	// The good files still landed.
	for _, n := range []string{"ok1.jpg", "ok2.jpg"} {
		if _, err := os.Stat(filepath.Join(dest, n)); err != nil {
			t.Errorf("%s missing: %v", n, err)
		}
	}
}

func TestMissingDevicePathFails(t *testing.T) {
	dest := t.TempDir()
	f := device.File{Name: "orphan.jpg", Size: 10, ID: "42", Coll: device.Images}
	res := run(t, &stub{sizes: map[string]int64{}}, []device.File{f}, dest, Skip)
	if len(res.Failed) != 1 {
		t.Fatalf("want 1 failure, got %d", len(res.Failed))
	}
}

func TestCancelStopsTransfer(t *testing.T) {
	dest := t.TempDir()
	var files []device.File
	sizes := map[string]int64{}
	for i := 0; i < 50; i++ {
		n := fmt.Sprintf("f%02d.jpg", i)
		files = append(files, file(n, 10, time.Now()))
		sizes["/sdcard/DCIM/Camera/"+n] = 10
	}
	dev := &stub{sizes: sizes, delay: 20 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan Event, 256)
	done := make(chan Result, 1)
	go func() { done <- Run(ctx, dev, files, dest, Skip, ch) }()
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()
	for range ch {
	}
	res := <-done

	if res.Pulled >= len(files) {
		t.Errorf("cancel did not stop the batch: pulled %d of %d", res.Pulled, len(files))
	}
}

func TestUniquePathIncrements(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.jpg")
	os.WriteFile(p, nil, 0o644)
	first := uniquePath(p)
	if filepath.Base(first) != "x (1).jpg" {
		t.Fatalf("first = %s", first)
	}
	os.WriteFile(first, nil, 0o644)
	if second := uniquePath(p); filepath.Base(second) != "x (2).jpg" {
		t.Errorf("second = %s", second)
	}
}
