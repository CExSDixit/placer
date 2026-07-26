package player

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// testPlayer drives the real process lifecycle against `sleep`, so pause,
// seek, speed changes and natural end are exercised without depending on
// ffplay being installed or on a file of a known length.
func testPlayer(t *testing.T, seconds string) *Player {
	t.Helper()
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	p := &Player{backend: BackendFFplay, bin: bin, speed: 1}
	p.argv = func(time.Duration) []string { return []string{seconds} }
	t.Cleanup(p.Stop)
	return p
}

func TestPlayer_NoBackendIsInert(t *testing.T) {
	p := New(func(string) string { return "" })
	if p.Available() {
		t.Fatal("a player with no backend must report unavailable")
	}
	// Every method must be safe to call anyway — the Audio tab's keys are
	// bound whether or not a player was found.
	p.Play("k", "n", "/tmp/x.mp3", time.Minute, 0)
	p.Toggle()
	p.Seek(time.Second)
	p.AdjustSpeed(0.25)
	p.Stop()
	if p.Playing() {
		t.Error("nothing should be playing")
	}
}

func TestPlayer_PrefersMPVThenFFplay(t *testing.T) {
	all := func(string) string { return "/usr/bin/x" }
	if got := New(all).Backend(); got != BackendMPV {
		t.Errorf("backend = %v, want mpv when everything is present", got)
	}
	noMPV := func(n string) string {
		if n == "mpv" {
			return ""
		}
		return "/usr/bin/x"
	}
	if got := New(noMPV).Backend(); got != BackendFFplay {
		t.Errorf("backend = %v, want ffplay when mpv is absent", got)
	}
	onlyAfplay := func(n string) string {
		if n == "afplay" {
			return "/usr/bin/afplay"
		}
		return ""
	}
	if got := New(onlyAfplay).Backend(); got != BackendAfplay {
		t.Errorf("backend = %v, want afplay as the last resort", got)
	}
}

func TestPlayer_PlayPauseResumeTracksPosition(t *testing.T) {
	p := testPlayer(t, "30")
	p.Play("/sdcard/a.mp3", "a.mp3", "/sdcard/a.mp3", 30*time.Second, 0)
	if !p.Playing() {
		t.Fatal("expected playback to start")
	}
	time.Sleep(120 * time.Millisecond)

	if pos := p.Position(); pos < 50*time.Millisecond {
		t.Errorf("playhead did not advance: %v", pos)
	}
	if p.Toggle() {
		t.Fatal("Toggle on a playing file should pause")
	}
	paused := p.Position()
	time.Sleep(80 * time.Millisecond)
	if got := p.Position(); got != paused {
		t.Errorf("playhead moved while paused: %v -> %v", paused, got)
	}
	if !p.Toggle() {
		t.Fatal("Toggle on a paused file should resume")
	}
	time.Sleep(80 * time.Millisecond)
	if got := p.Position(); got <= paused {
		t.Errorf("playhead did not resume from %v, got %v", paused, got)
	}
}

func TestPlayer_SeekRestartsAtNewOffset(t *testing.T) {
	p := testPlayer(t, "30")
	p.Play("k", "a.mp3", "/sdcard/a.mp3", 5*time.Minute, 0)
	time.Sleep(50 * time.Millisecond)

	p.Seek(30 * time.Second)
	if got := p.Position(); got < 29*time.Second {
		t.Errorf("after +30s seek, position = %v", got)
	}
	if !p.Playing() {
		t.Error("seeking while playing must keep playing")
	}

	// Seeking below zero clamps rather than passing a negative -ss. It keeps
	// playing from the top, so the playhead is at 0 plus however long the
	// assertion took to run.
	p.Seek(-10 * time.Minute)
	if got := p.Position(); got > 50*time.Millisecond {
		t.Errorf("position = %v, want ~0 after seeking past the start", got)
	}

	// Seeking past the end stops rather than restarting a zero-length tail.
	p.Seek(10 * time.Minute)
	if p.Playing() {
		t.Error("seeking past the end should stop playback")
	}
	if got := p.Position(); got != 5*time.Minute {
		t.Errorf("position = %v, want the duration", got)
	}
}

func TestPlayer_SpeedClampsAndPreservesPosition(t *testing.T) {
	p := testPlayer(t, "30")
	p.Play("k", "a.mp3", "/sdcard/a.mp3", 10*time.Minute, 0)
	p.Seek(60 * time.Second)

	before := p.Position()
	if got := p.AdjustSpeed(SpeedStep); got != 1+SpeedStep {
		t.Errorf("speed = %v, want %v", got, 1+SpeedStep)
	}
	// Changing speed must not rescale the elapsed time retroactively — the
	// playhead should be where it was, then advance faster.
	if after := p.Position(); after < before || after > before+time.Second {
		t.Errorf("speed change moved the playhead: %v -> %v", before, after)
	}

	for i := 0; i < 20; i++ {
		p.AdjustSpeed(SpeedStep)
	}
	if got := p.Speed(); got != MaxSpeed {
		t.Errorf("speed = %v, want clamp at %v", got, MaxSpeed)
	}
	for i := 0; i < 20; i++ {
		p.AdjustSpeed(-SpeedStep)
	}
	if got := p.Speed(); got != MinSpeed {
		t.Errorf("speed = %v, want clamp at %v", got, MinSpeed)
	}
}

// TestPlayer_ReaperDoesNotClearItsSuccessor is the race the gen counter
// exists for: a seek kills the running process, and that process's reaper
// goroutine must not then mark the *replacement* as stopped.
func TestPlayer_ReaperDoesNotClearItsSuccessor(t *testing.T) {
	p := testPlayer(t, "30")
	p.Play("k", "a.mp3", "/sdcard/a.mp3", 10*time.Minute, 0)
	for i := 0; i < 50; i++ {
		p.Seek(time.Second)
	}
	// Give every retired reaper time to run and try to write back.
	time.Sleep(200 * time.Millisecond)
	if !p.Playing() {
		t.Fatal("a killed process's reaper cleared the playing flag of its successor")
	}
}

func TestPlayer_EndOfTrackParksPlayhead(t *testing.T) {
	p := testPlayer(t, "0.1")
	p.Play("k", "a.mp3", "/sdcard/a.mp3", 90*time.Second, 0)
	deadline := time.Now().Add(3 * time.Second)
	for p.Playing() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if p.Playing() {
		t.Fatal("playback did not end on its own")
	}
	if got := p.Position(); got != 90*time.Second {
		t.Errorf("position = %v, want the duration once the track ends", got)
	}
}

func TestPlayer_StopForgetsTheFile(t *testing.T) {
	p := testPlayer(t, "30")
	p.Play("/sdcard/a.mp3", "a.mp3", "/sdcard/a.mp3", time.Minute, 0)
	if k, _ := p.Loaded(); k != "/sdcard/a.mp3" {
		t.Fatalf("Loaded = %q", k)
	}
	p.Stop()
	if k, n := p.Loaded(); k != "" || n != "" {
		t.Errorf("Loaded = %q/%q after Stop", k, n)
	}
	if p.Toggle() {
		t.Error("Toggle with nothing loaded should not start playback")
	}
}

func TestPlayer_Args(t *testing.T) {
	mk := func(b Backend, speed float64) *Player {
		return &Player{backend: b, bin: "x", path: "/tmp/a.mp3", speed: speed}
	}
	ff := strings.Join(mk(BackendFFplay, 1).args(90*time.Second), " ")
	for _, want := range []string{"-nodisp", "-autoexit", "-ss 90.00", "/tmp/a.mp3"} {
		if !strings.Contains(ff, want) {
			t.Errorf("ffplay args %q missing %q", ff, want)
		}
	}
	if strings.Contains(ff, "atempo") {
		t.Error("no atempo filter should be added at 1x")
	}
	// atempo is only valid over [0.5, 2.0] — exactly the range AdjustSpeed
	// clamps to, so a single filter always suffices.
	if got := strings.Join(mk(BackendFFplay, 1.5).args(0), " "); !strings.Contains(got, "atempo=1.50") {
		t.Errorf("ffplay args %q missing atempo", got)
	}
	if got := strings.Join(mk(BackendMPV, 1.25).args(30*time.Second), " "); !strings.Contains(got, "--start=30.00") ||
		!strings.Contains(got, "--speed=1.25") || !strings.Contains(got, "--no-video") {
		t.Errorf("mpv args = %q", got)
	}
	if got := strings.Join(mk(BackendAfplay, 1.25).args(0), " "); !strings.Contains(got, "-r 1.25") {
		t.Errorf("afplay args = %q", got)
	}
}
