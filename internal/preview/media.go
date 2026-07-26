package preview

import (
	"os/exec"
	"sync"
)

// The external-tool contract, unchanged since phase 1: `adb` is hard,
// `ffmpeg` is soft. Everything video and audio degrades to a metadata card
// when ffmpeg is absent — it never errors, never hangs, and never becomes a
// third required dependency. `mpv` and `afplay` are opportunistic playback
// fallbacks only (see internal/player); nothing here requires them.
var (
	toolOnce sync.Once
	toolPath = map[string]string{}
)

// Tool returns the absolute path of an external tool, or "" if it is not on
// PATH. Resolved once per process: LookPath hits the filesystem, and this is
// consulted on every cursor rest.
func Tool(name string) string {
	toolOnce.Do(func() {
		for _, n := range []string{"ffmpeg", "ffprobe", "ffplay", "mpv", "afplay"} {
			if p, err := exec.LookPath(n); err == nil {
				toolPath[n] = p
			}
		}
	})
	return toolPath[name]
}

// HasFFmpeg reports whether frame grabs and waveforms are possible at all.
func HasFFmpeg() bool { return Tool("ffmpeg") != "" }

// HasFFprobe reports whether container metadata can be read.
func HasFFprobe() bool { return Tool("ffprobe") != "" }
