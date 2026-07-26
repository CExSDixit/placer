// Package player drives audio playback by shelling out to a media player,
// with the TUI owning the playhead.
//
// There is no pure-Go decoder here on purpose. `ebitengine/oto` was tested and
// killed during scoping: it needs CGO+ALSA on Linux, which breaks placer's
// CGO_ENABLED=0 single-binary constraint. Shelling out costs a process per
// play and makes pause/seek crude — pause kills the process and remembers the
// offset, seek kills and restarts with a new -ss — but it is ~40 lines with no
// platform-specific code, and it keeps the dependency list at adb + ffmpeg.
package player

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// Backend is which external player was found. ffplay ships with ffmpeg, which
// placer already treats as a soft dependency; mpv is used opportunistically
// when present (it seeks without the audible gap a process restart causes);
// afplay is the macOS fallback for a machine with no ffmpeg at all. None of
// the three is ever required.
type Backend int

const (
	BackendNone Backend = iota
	BackendFFplay
	BackendMPV
	BackendAfplay
)

func (b Backend) String() string {
	switch b {
	case BackendFFplay:
		return "ffplay"
	case BackendMPV:
		return "mpv"
	case BackendAfplay:
		return "afplay"
	}
	return "none"
}

const (
	MinSpeed  = 0.5
	MaxSpeed  = 2.0
	SpeedStep = 0.25
)

// Player owns at most one playback process at a time. Every field is guarded
// by mu: the Bubble Tea update loop calls in from one goroutine, but the
// process reaper goroutine writes back from another.
type Player struct {
	mu      sync.Mutex
	backend Backend
	bin     string

	cancel context.CancelFunc

	// gen increments on every state change that invalidates a running
	// process. The reaper goroutine captures the gen it was started with and
	// only writes back if it still matches, so a process killed by a seek can
	// never clear the `playing` flag that the *replacement* process just set.
	gen int

	key     string // device path of the file loaded, "" if none
	name    string
	path    string // local file being played
	dur     time.Duration
	offset  time.Duration // media position the current process started from
	started time.Time     // wall clock when it started
	playing bool
	speed   float64

	// argv overrides the backend's command line. Tests set it so the
	// lifecycle — pause, seek, speed change, natural end — can be driven
	// against a process they control, rather than depending on ffplay being
	// installed and on a file that takes a known time to play.
	argv func(at time.Duration) []string
}

// New picks a backend, preferring mpv (gapless seek) then ffplay then afplay.
func New(lookup func(string) string) *Player {
	p := &Player{speed: 1.0}
	for _, c := range []struct {
		bin string
		b   Backend
	}{{"mpv", BackendMPV}, {"ffplay", BackendFFplay}, {"afplay", BackendAfplay}} {
		if path := lookup(c.bin); path != "" {
			p.bin, p.backend = path, c.b
			break
		}
	}
	return p
}

func (p *Player) Available() bool { return p != nil && p.backend != BackendNone }

func (p *Player) Backend() Backend {
	if p == nil {
		return BackendNone
	}
	return p.backend
}

// Loaded reports the device path and display name of the file the player is
// holding, playing or paused.
func (p *Player) Loaded() (key, name string) {
	if p == nil {
		return "", ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.key, p.name
}

func (p *Player) Playing() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playing
}

func (p *Player) Speed() float64 {
	if p == nil {
		return 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.speed
}

func (p *Player) Duration() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dur
}

// Position is the playhead: the offset the current process started from plus
// the wall time since, scaled by playback speed.
func (p *Player) Position() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.positionLocked()
}

func (p *Player) positionLocked() time.Duration {
	pos := p.offset
	if p.playing {
		pos += time.Duration(float64(time.Since(p.started)) * p.speed)
	}
	if p.dur > 0 && pos > p.dur {
		pos = p.dur
	}
	if pos < 0 {
		pos = 0
	}
	return pos
}

// Play loads a file and starts it from `at`. Any current playback is stopped
// first — one process at a time, always.
func (p *Player) Play(key, name, path string, dur, at time.Duration) {
	if !p.Available() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.key, p.name, p.path, p.dur = key, name, path, dur
	p.startLocked(at)
}

// Toggle pauses if playing, resumes if paused. Reports whether it is playing
// afterwards.
func (p *Player) Toggle() bool {
	if !p.Available() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path == "" {
		return false
	}
	if p.playing {
		at := p.positionLocked()
		p.killLocked()
		p.offset = at
		return false
	}
	p.startLocked(p.offset)
	return true
}

// Seek moves the playhead by delta, restarting the process there when
// playing. Seeking a paused file just moves the remembered offset.
func (p *Player) Seek(delta time.Duration) {
	if !p.Available() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path == "" {
		return
	}
	at := p.positionLocked() + delta
	if at < 0 {
		at = 0
	}
	if p.dur > 0 && at >= p.dur {
		// Seeking past the end is a stop, not a restart of a zero-length tail.
		p.killLocked()
		p.offset = p.dur
		return
	}
	if p.playing {
		p.startLocked(at)
		return
	}
	p.offset = at
}

// AdjustSpeed steps playback speed within [MinSpeed, MaxSpeed] and restarts
// the process so the change takes effect immediately.
func (p *Player) AdjustSpeed(delta float64) float64 {
	if !p.Available() {
		return 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	at := p.positionLocked()
	s := p.speed + delta
	if s < MinSpeed {
		s = MinSpeed
	}
	if s > MaxSpeed {
		s = MaxSpeed
	}
	if s == p.speed {
		return s
	}
	// Snapshot the position at the OLD speed before changing it, or the
	// elapsed wall time gets rescaled retroactively and the playhead jumps.
	p.speed = s
	if p.playing {
		p.startLocked(at)
	} else {
		p.offset = at
	}
	return s
}

// Stop ends playback and forgets the loaded file.
func (p *Player) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killLocked()
	p.key, p.name, p.path, p.offset, p.dur = "", "", "", 0, 0
}

// killLocked terminates any running process and marks the player stopped.
// Bumping gen first is what makes the reaper for the dying process a no-op.
func (p *Player) killLocked() {
	p.gen++
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.playing = false
}

func (p *Player) startLocked(at time.Duration) {
	p.killLocked() // also bumps gen, retiring the previous reaper
	if p.path == "" {
		return
	}
	if at < 0 {
		at = 0
	}

	argv := p.args
	if p.argv != nil {
		argv = p.argv
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, p.bin, argv(at)...)
	if err := cmd.Start(); err != nil {
		cancel()
		return
	}
	p.cancel = cancel
	p.offset = at
	p.started = time.Now()
	p.playing = true

	gen := p.gen
	go func() {
		_ = cmd.Wait()
		cancel()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.gen != gen {
			return // superseded by a seek, a speed change or another file
		}
		// Reached the end on its own: park the playhead there rather than
		// letting Position keep advancing off a stale `started`.
		p.playing = false
		p.offset = p.dur
		p.cancel = nil
	}()
}

func (p *Player) args(at time.Duration) []string {
	sec := at.Seconds()
	switch p.backend {
	case BackendMPV:
		return []string{"--no-video", "--really-quiet",
			fmtArg("--start=%.2f", sec), fmtArg("--speed=%.2f", p.speed), p.path}
	case BackendFFplay:
		a := []string{"-nodisp", "-autoexit", "-loglevel", "quiet",
			"-ss", fmtNum(sec), "-i", p.path}
		if p.speed != 1 {
			// atempo is valid over [0.5, 2.0], which is exactly the range
			// AdjustSpeed clamps to, so one filter always suffices.
			a = append(a, "-af", fmtArg("atempo=%.2f", p.speed))
		}
		return a
	default: // afplay: no seek support, so `at` is best-effort from the top
		return []string{"-r", fmtNum(p.speed), p.path}
	}
}
