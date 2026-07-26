package preview

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

// TestSparseRegions_HeaderIsNeverAHole: ffmpeg's mov probe reads `ftyp` at
// offset 0 to identify the container at all, so however deep the scrub goes,
// the first region must still start at 0.
func TestSparseRegions_HeaderIsNeverAHole(t *testing.T) {
	f := device.File{Size: 894471198, Duration: 324 * time.Second}
	for _, at := range []time.Duration{0, time.Second, 30 * time.Second, 5 * time.Minute} {
		regs := sparseRegions(f, at, sparseTail)
		if regs[0].off != 0 {
			t.Errorf("at=%v: first region starts at %d, not 0", at, regs[0].off)
		}
		last := regs[len(regs)-1]
		if last.off+last.length != f.Size {
			t.Errorf("at=%v: last region ends at %d, file is %d", at, last.off+last.length, f.Size)
		}
		for i, r := range regs {
			if r.off%mib != 0 {
				t.Errorf("at=%v region %d: offset %d is not mebibyte-aligned", at, i, r.off)
			}
		}
		// Regions must be disjoint and ordered, or dd would fetch the same
		// megabyte twice and WriteAt would overwrite good bytes.
		for i := 1; i < len(regs); i++ {
			if regs[i].off < regs[i-1].off+regs[i-1].length {
				t.Errorf("at=%v: region %d overlaps its predecessor", at, i)
			}
		}
	}
}

// TestSparseRegions_WindowTracksTheSeek: a deep scrub must fetch bytes from
// where the frame actually lives, not just more of the head.
func TestSparseRegions_WindowTracksTheSeek(t *testing.T) {
	f := device.File{Size: 894471198, Duration: 324 * time.Second}
	bps := float64(f.Size) / f.Duration.Seconds()

	for _, at := range []time.Duration{60 * time.Second, 200 * time.Second} {
		regs := sparseRegions(f, at, sparseTail)
		want := int64(bps * at.Seconds())
		var covered bool
		for _, r := range regs {
			if want >= r.off && want < r.off+r.length {
				covered = true
			}
		}
		if !covered {
			t.Errorf("at=%v: byte offset %d is not inside any fetched region %+v", at, want, regs)
		}
	}

	// Still sparse: a scrub fetches three small windows, not the file.
	regs := sparseRegions(f, 200*time.Second, sparseTail)
	var total int64
	for _, r := range regs {
		total += r.length
	}
	if total > f.Size/8 {
		t.Errorf("scrub fetches %d of %d bytes — that is not sparse", total, f.Size)
	}
}

// TestSparseRegions_ShortClipMerges: a seek inside a small file collapses to
// one span, which is the signal to do a full pull instead.
func TestSparseRegions_ShortClipMerges(t *testing.T) {
	f := device.File{Size: 6 * mib, Duration: 10 * time.Second}
	if regs := sparseRegions(f, 5*time.Second, sparseTail); len(regs) != 1 {
		t.Errorf("a 6 MiB file produced %d regions, want 1 merged span: %+v", len(regs), regs)
	}
}

// TestScrubbedFrameMatchesFullPull is the scrub equivalent of the phase 3
// headline claim: the frame from a windowed reconstruction at 1:00 is the
// frame a full pull gives at 1:00.
func TestScrubbedFrameMatchesFullPull(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	dir, f := largeVideo(t)
	dev := &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}
	full := filepath.Join(dir, largeSample)

	for _, at := range []time.Duration{60 * time.Second, 180 * time.Second} {
		dst := filepath.Join(t.TempDir(), "sparse.mp4")
		moved, err := buildSparse(context.Background(), dev, f, dst, at, sparseTail)
		if err != nil {
			t.Fatalf("at=%v buildSparse: %v", at, err)
		}
		if moved >= f.Size/4 {
			t.Errorf("at=%v: transferred %d of %d bytes", at, moved, f.Size)
		}

		sparseFrame, err := GrabFrameAt(context.Background(), dst, at)
		if err != nil {
			t.Fatalf("at=%v GrabFrameAt(sparse): %v", at, err)
		}
		fullFrame, err := GrabFrameAt(context.Background(), full, at)
		if err != nil {
			t.Fatalf("at=%v GrabFrameAt(full): %v", at, err)
		}
		if sha256.Sum256(sparseFrame) != sha256.Sum256(fullFrame) {
			t.Errorf("at=%v: scrubbed frame differs from the full-pull frame "+
				"(sparse %d B, full %d B)", at, len(sparseFrame), len(fullFrame))
		}
	}
}

// TestFrameVariantSeparatesCacheEntries: without this, scrubbing would keep
// serving the frame cached at the previous position.
func TestFrameVariantSeparatesCacheEntries(t *testing.T) {
	f := device.File{Path: "/sdcard/a.mp4", Size: 100, Mime: "video/mp4"}
	a := cacheKey(f, 40, 20, ProtoHalfBlock, frameVariant(time.Second))
	b := cacheKey(f, 40, 20, ProtoHalfBlock, frameVariant(30*time.Second))
	if a == b {
		t.Error("frames at different seek points share a cache key")
	}
	if a != cacheKey(f, 40, 20, ProtoHalfBlock, frameVariant(time.Second)) {
		t.Error("the same seek point produced two different cache keys")
	}
}

// countingDevice records how many pulls actually reached the device.
type countingDevice struct {
	*device.Fake
	mu sync.Mutex
	n  int
}

func (d *countingDevice) Pull(ctx context.Context, remote, local string, prog func(device.Progress)) error {
	d.mu.Lock()
	d.n++
	d.mu.Unlock()
	return d.Fake.Pull(ctx, remote, local, prog)
}

func (d *countingDevice) pulls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

// TestEnsureLocal_SharesOnePull is the fix for the bug real use surfaced:
// cursor rest fires the waveform fetch and the autoplay load at the same
// instant, both wanting the same file. They used to race on one temp path,
// pull a 150 MB voice memo twice, and whichever lost reported
// "rename: no such file or directory" and never started playing.
func TestEnsureLocal_SharesOnePull(t *testing.T) {
	dir := sampleDir(t)
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, ".cache"))
	dev := &countingDevice{Fake: &device.Fake{SrcDir: dir, Latency: 5 * time.Millisecond, Speed: 8 << 20}}

	f := sampleFile(t, dir, "Zombie_In_Pain-SoundBible.com-134322253.wav", "audio/x-wav")

	const callers = 6
	var wg sync.WaitGroup
	paths := make([]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = EnsureLocal(context.Background(), dev, f)
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if paths[i] != paths[0] {
			t.Errorf("caller %d got %q, caller 0 got %q", i, paths[i], paths[0])
		}
	}
	if got := dev.pulls(); got != 1 {
		t.Errorf("%d concurrent callers caused %d pulls, want 1", callers, got)
	}
	st, err := os.Stat(paths[0])
	if err != nil || st.Size() != f.Size {
		t.Fatalf("result is not a complete file: %v", err)
	}
	// No temp files left over — a stray .partial would be handed to ffplay
	// by a later run.
	entries, _ := os.ReadDir(MediaCacheDir())
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("media cache holds %v, want just the finished file", names)
	}
}

// TestEnsureLocal_OneCancelDoesNotKillTheOther: the waveform fetch and the
// playback load are cancelled independently, so one giving up must not
// abort the pull the other is still waiting on.
func TestEnsureLocal_OneCancelDoesNotKillTheOther(t *testing.T) {
	dir := sampleDir(t)
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, ".cache"))
	dev := &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 2 << 20}
	f := sampleFile(t, dir, "Zombie_In_Pain-SoundBible.com-134322253.wav", "audio/x-wav")

	quitter, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := EnsureLocal(context.Background(), dev, f)
		done <- err
	}()
	<-started
	// Join the same in-flight pull, then abandon it.
	go func() { _, _ = EnsureLocal(quitter, dev, f) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the remaining waiter was starved by the other's cancel: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("pull never completed")
	}
}

func TestPruneCaches_TrimsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, size int, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldest := mk("old.wav", 400, 3*time.Hour)
	middle := mk("mid.wav", 400, 2*time.Hour)
	newest := mk("new.wav", 400, time.Hour)
	stale := mk(".partial-abc", 400, time.Minute)

	pruneDir(dir, 900)

	if _, err := os.Stat(stale); err == nil {
		t.Error("a leftover .partial should always be removed")
	}
	if _, err := os.Stat(oldest); err == nil {
		t.Error("the oldest file should have been evicted first")
	}
	if _, err := os.Stat(newest); err != nil {
		t.Error("the newest file should have been kept")
	}
	_ = middle
}

func TestPruneCaches_UnderBudgetKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.wav")
	if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneDir(dir, 1<<20)
	if _, err := os.Stat(p); err != nil {
		t.Error("pruning under budget deleted a file")
	}
}
