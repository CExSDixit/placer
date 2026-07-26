package preview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

func TestFetchAudio_WAV(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "Zombie_In_Pain-SoundBible.com-134322253.wav", "audio/x-wav")
	f.Coll = device.Audio

	res, err := Fetch(context.Background(), dev, f, 40, 12, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierAudio {
		t.Fatalf("tier = %v, want TierAudio (note %q)", res.Tier, res.Note)
	}
	if len(res.Rendered) == 0 {
		t.Errorf("no waveform rendered: %q", res.Note)
	}
	if res.Local == "" {
		t.Error("audio preview must report the local path, so playback reuses the pull")
	}
	if _, err := os.Stat(res.Local); err != nil {
		t.Errorf("reported local path is not readable: %v", err)
	}
	if res.Duration <= 0 {
		t.Error("ffprobe should have supplied a duration MediaStore did not have")
	}

	// Second fetch hits the thumb cache and must not change what it reports.
	res2, err := Fetch(context.Background(), dev, f, 40, 12, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if string(res2.Rendered) != string(res.Rendered) {
		t.Error("cached waveform differs from the first render")
	}
}

func TestFetchAudio_MP3(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "Slack - Yoink.mp3", "audio/mpeg")
	f.Coll = device.Audio

	res, err := Fetch(context.Background(), dev, f, 40, 12, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierAudio || len(res.Rendered) == 0 {
		t.Fatalf("got tier=%v rendered=%d note=%q", res.Tier, len(res.Rendered), res.Note)
	}
}

func TestEnsureLocal_ReusesAndIsAtomic(t *testing.T) {
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "Slack - Yoink.mp3", "audio/mpeg")

	local, err := EnsureLocal(context.Background(), dev, f)
	if err != nil {
		t.Fatalf("EnsureLocal: %v", err)
	}
	st, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != f.Size {
		t.Fatalf("local size %d, want %d", st.Size(), f.Size)
	}

	// A second call must not re-pull: mark the file and check it survives.
	marker := st.ModTime().Add(-time.Hour)
	if err := os.Chtimes(local, marker, marker); err != nil {
		t.Fatal(err)
	}
	again, err := EnsureLocal(context.Background(), dev, f)
	if err != nil || again != local {
		t.Fatalf("EnsureLocal again = %q, %v", again, err)
	}
	st2, _ := os.Stat(again)
	if !st2.ModTime().Equal(marker) {
		t.Error("EnsureLocal re-pulled a file it already had")
	}
}

func TestEnsureLocal_CancelLeavesNoPartial(t *testing.T) {
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "Zombie_In_Pain-SoundBible.com-134322253.wav", "audio/x-wav")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := EnsureLocal(ctx, dev, f); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}

	// The cache path itself must never hold a truncated file — that is what
	// would be handed to ffplay on the next press of space. Checked
	// immediately, because the rename only happens on a complete pull.
	if _, err := os.Stat(LocalPath(f)); err == nil {
		t.Error("a cancelled pull left a file at the cache path")
	}

	// The abandoned pull's goroutine outlives the cancelled caller by design —
	// cancellation returns promptly rather than blocking on teardown — so
	// drain it with a completing call before asserting on the directory.
	local, err := EnsureLocal(context.Background(), dev, f)
	if err != nil {
		t.Fatalf("EnsureLocal after a cancel: %v", err)
	}
	entries, _ := os.ReadDir(MediaCacheDir())
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("media cache holds %v, want only %s", names, filepath.Base(local))
	}
}

func TestProbeLines_FlagsDurationDisagreement(t *testing.T) {
	// MediaStore's `duration` is written by whatever app produced the file;
	// ffprobe reads the container. When they disagree, say so rather than
	// silently preferring one.
	f := device.File{Duration: 30 * time.Second}
	got := probeLines(f, Probe{Duration: 45 * time.Second, Codec: "aac"})
	var found bool
	for _, l := range got {
		if len(l) > 5 && l[:5] == "probe" {
			found = true
		}
	}
	if !found {
		t.Errorf("a 15s disagreement was not reported: %q", got)
	}

	// Agreement within a second is not worth a line.
	got = probeLines(f, Probe{Duration: 30*time.Second + 200*time.Millisecond})
	for _, l := range got {
		if len(l) > 5 && l[:5] == "probe" {
			t.Errorf("agreeing durations should not be flagged: %q", got)
		}
	}
}

func TestMetaLines_CoversWhatMediaStoreKnows(t *testing.T) {
	f := device.File{
		Size: 12 << 20, Mime: "video/mp4", Bucket: "Camera",
		Duration: 90 * time.Second, Added: time.Unix(1750000000, 0),
	}
	lines := MetaLines(f)
	if len(lines) < 5 {
		t.Fatalf("thin metadata card: %q", lines)
	}
	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	for _, want := range []string{"size", "length", "type", "album", "bitrate"} {
		if !contains(joined, want) {
			t.Errorf("metadata card missing %q: %q", want, lines)
		}
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
