// Package preview renders inline images and metadata cards for the file
// list: pull the whole file (an EXIF head-bytes fast path was measured and
// cut — adb pull is already faster than a partial fetch), decode,
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

// cacheKey is path + size + date_added + cell geometry + protocol + variant:
// any of those changing means either the bytes on the device changed (or a
// different file landed at the same path) or the rendered bytes wouldn't
// suit this terminal/pane anymore, so the cached render must not be reused.
//
// variant distinguishes several renders of the SAME file at the same
// geometry — currently the video frame's seek point, so scrubbing to 1:00
// never serves the frame cached at 0:30.
func cacheKey(f device.File, cellW, cellH int, proto Protocol, variant ...string) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s|%d|%d|%dx%d|%s", f.Path, f.Size, f.Added.Unix(), cellW, cellH, proto)
	for _, v := range variant {
		// Empty variants are skipped so that an explicit "" and an omitted
		// argument hash identically. They did not, and the mismatch meant
		// writeCache (which always passed a variant, "" for images) and
		// readCache (which passed none) computed different keys — so image
		// previews silently stopped being cached at all and every cursor rest
		// re-pulled and re-rendered.
		if v == "" {
			continue
		}
		fmt.Fprintf(h, "|%s", v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// renderVersion invalidates every cached render when the bytes we emit change.
//
// The cache stores *rendered protocol bytes*, keyed by file, geometry and
// protocol — but not by how those bytes were produced. Without this, changing
// what Render emits leaves previews coming back from disk in the old encoding
// forever. Found while investigating something else: nine entries in
// ~/.cache/placer/thumbs still carried a kitty header from an earlier
// experiment (`grep -l i=7301`).
//
// **Bump this on any change to what Render emits.** Old entries become
// unreachable and age out of the budget on their own, so nobody has to know
// to clear a cache.
const renderVersion = "r3"

func cachePath(f device.File, cellW, cellH int, proto Protocol, variant ...string) string {
	// Deliberately applied here and not in cacheKey: the media cache keys
	// whole pulled files off cacheKey too, and those are hundreds of
	// megabytes each. A render-format bump must not force them to be
	// re-pulled — it has nothing to do with them.
	key := cacheKey(f, cellW, cellH, proto, append([]string{renderVersion}, variant...)...)
	return filepath.Join(CacheDir(), key+".cache")
}

func readCache(f device.File, cellW, cellH int, proto Protocol, variant ...string) ([]byte, bool) {
	b, err := os.ReadFile(cachePath(f, cellW, cellH, proto, variant...))
	if err != nil {
		return nil, false
	}
	return b, true
}

func writeCache(f device.File, cellW, cellH int, proto Protocol, variant string, rendered []byte) {
	p := cachePath(f, cellW, cellH, proto, variant)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, rendered, 0o644)
}
