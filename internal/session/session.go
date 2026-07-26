// Package session holds the selection manifest and user config, both
// persisted so a crash or an accidental quit never loses curation work.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/CExSDixit/placer/internal/device"
)

// Manifest is the set of files chosen for transfer. It is keyed by device
// path and deliberately independent of the current tab, filter and directory
// — selections must survive all of those, which is the entire point.
type Manifest struct {
	files map[string]device.File
	order []string // insertion order, so the review pane is stable
}

func NewManifest() *Manifest {
	return &Manifest{files: map[string]device.File{}}
}

func key(f device.File) string {
	if f.Path != "" {
		return f.Path
	}
	return f.ContentURI()
}

func (m *Manifest) Has(f device.File) bool {
	_, ok := m.files[key(f)]
	return ok
}

// Toggle adds or removes a file, reporting whether it is now selected.
func (m *Manifest) Toggle(f device.File) bool {
	k := key(f)
	if _, ok := m.files[k]; ok {
		delete(m.files, k)
		for i, o := range m.order {
			if o == k {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
		return false
	}
	m.files[k] = f
	m.order = append(m.order, k)
	return true
}

func (m *Manifest) Add(f device.File) {
	if !m.Has(f) {
		m.Toggle(f)
	}
}

func (m *Manifest) Remove(f device.File) {
	if m.Has(f) {
		m.Toggle(f)
	}
}

func (m *Manifest) Len() int { return len(m.files) }

func (m *Manifest) Clear() {
	m.files = map[string]device.File{}
	m.order = nil
}

// Files returns the selection in insertion order.
func (m *Manifest) Files() []device.File {
	out := make([]device.File, 0, len(m.order))
	for _, k := range m.order {
		if f, ok := m.files[k]; ok {
			out = append(out, f)
		}
	}
	return out
}

func (m *Manifest) TotalBytes() int64 {
	var n int64
	for _, f := range m.files {
		n += f.Size
	}
	return n
}

type persisted struct {
	Files []device.File `json:"files"`
}

func (m *Manifest) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(persisted{Files: m.Files()}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func LoadManifest(path string) *Manifest {
	m := NewManifest()
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var p persisted
	if json.Unmarshal(b, &p) != nil {
		return m
	}
	for _, f := range p.Files {
		m.Add(f)
	}
	return m
}

// Config is the small amount of user state worth remembering between runs.
type Config struct {
	Dest      string   `json:"dest"`
	Bookmarks []string `json:"bookmarks"`
	Recents   []string `json:"recents"`

	// Preview toggles, independent and switchable at runtime via `:set`, so
	// nobody ever has to hold a key down to stop something playing.
	Preview  bool `json:"preview"`  // image preview on cursor rest
	Autoplay bool `json:"autoplay"` // video frame-grab autoplay on cursor rest
	Audio    bool `json:"audio"`    // audio auto-play on j/k (phase 3)

	// Render overrides the detected terminal image protocol, e.g. to fall
	// back from quadrant blocks to half-blocks on a font that draws the
	// quadrant glyphs with gaps. Empty means "use whatever was detected".
	Render string `json:"render,omitempty"`
}

func CacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ".placer"
	}
	return filepath.Join(d, "placer")
}

func ConfigPath() string   { return filepath.Join(CacheDir(), "config.json") }
func ManifestPath() string { return filepath.Join(CacheDir(), "session.json") }

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	c := Config{
		Dest:     filepath.Join(home, "Downloads"),
		Preview:  true,  // Sid's explicit request
		Autoplay: false, // confirmed by Sid: a frame grab firing on every cursor move fights j/k
		Audio:    true,  // the scrub-through-voice-memos flow (phase 3)
	}
	// Only well-known directories; anything personal belongs in config.json,
	// which the destination picker writes as you use it.
	for _, p := range []string{
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Pictures"),
		filepath.Join(home, "Desktop"),
	} {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			c.Bookmarks = append(c.Bookmarks, p)
		}
	}
	return c
}

func LoadConfig() Config {
	c := DefaultConfig()
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return c
	}
	var got Config
	if json.Unmarshal(b, &got) != nil {
		return c
	}
	if got.Dest != "" {
		c.Dest = got.Dest
	}
	if len(got.Bookmarks) > 0 {
		c.Bookmarks = got.Bookmarks
	}
	c.Recents = got.Recents
	c.Render = got.Render

	// The preview toggles default true/false/true; only override them from a
	// config.json that actually mentions the key, so an old config file
	// written before these existed doesn't silently flip them all off.
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) == nil {
		if _, ok := raw["preview"]; ok {
			c.Preview = got.Preview
		}
		if _, ok := raw["autoplay"]; ok {
			c.Autoplay = got.Autoplay
		}
		if _, ok := raw["audio"]; ok {
			c.Audio = got.Audio
		}
	}
	return c
}

func (c Config) Save() error {
	if err := os.MkdirAll(CacheDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), b, 0o644)
}

// NoteRecent moves dir to the front of the recents list, capped at 5.
func (c *Config) NoteRecent(dir string) {
	out := []string{dir}
	for _, r := range c.Recents {
		if r != dir && len(out) < 5 {
			out = append(out, r)
		}
	}
	sort.SliceStable(out[1:], func(i, j int) bool { return false })
	c.Recents = out
	c.Dest = dir
}
