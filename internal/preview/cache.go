// Package preview renders inline images and metadata cards for the file
// list: pull the whole file (the EXIF head-bytes fast path was measured and
// cut — adb pull is already faster than a partial fetch, see
// rabbitholes/adb-fuzzy-file-browser-implementation-scope.md), decode,
// downscale, cache, and render through whatever protocol the terminal
// supports, falling back to Unicode half-blocks everywhere else.
package preview

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CExSDixit/placer/internal/device"
)

func cacheRoot() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ".placer-cache"
	}
	return filepath.Join(d, "placer")
}

// CacheDir is where rendered thumbnails live, keyed so a file is never
// re-fetched or re-decoded across restarts.
func CacheDir() string {
	return filepath.Join(cacheRoot(), "thumbs")
}

// cacheKey is path + size + date_added + cell geometry + protocol: any of
// those changing means either the bytes on the device changed (or a
// different file landed at the same path) or the rendered bytes wouldn't
// suit this terminal/pane anymore, so the cached render must not be reused.
func cacheKey(f device.File, cellW, cellH int, proto Protocol) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s|%d|%d|%dx%d|%s", f.Path, f.Size, f.Added.Unix(), cellW, cellH, proto)
	return hex.EncodeToString(h.Sum(nil))
}

func cachePath(f device.File, cellW, cellH int, proto Protocol) string {
	return filepath.Join(CacheDir(), cacheKey(f, cellW, cellH, proto)+".cache")
}

func readCache(f device.File, cellW, cellH int, proto Protocol) ([]byte, bool) {
	b, err := os.ReadFile(cachePath(f, cellW, cellH, proto))
	if err != nil {
		return nil, false
	}
	return b, true
}

func writeCache(f device.File, cellW, cellH int, proto Protocol, rendered []byte) {
	p := cachePath(f, cellW, cellH, proto)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, rendered, 0o644)
}
