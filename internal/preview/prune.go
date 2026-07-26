package preview

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Cache budgets. The media cache holds whole audio files, and on the
// reference device voice memos run 130–300 MB each — ten minutes of ordinary
// browsing put 758 MB on disk. Without a cap it grows forever, so it is
// pruned oldest-first at startup.
//
// The thumb cache is rendered escape sequences, kilobytes each, and is
// capped mostly to stop a decade of it accumulating.
const (
	MediaCacheBudget = 2 << 30   // 2 GiB
	ThumbCacheBudget = 256 << 20 // 256 MiB
)

// PruneCaches trims both caches to their budgets, deleting least-recently
// modified files first. Errors are ignored: a cache that cannot be pruned is
// not a reason to refuse to start.
func PruneCaches() {
	pruneDir(MediaCacheDir(), MediaCacheBudget)
	pruneDir(CacheDir(), ThumbCacheBudget)
}

func pruneDir(dir string, budget int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	type item struct {
		path string
		size int64
		mod  int64
	}
	var items []item
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		// A .partial-* left behind by a killed pull is never useful again.
		if strings.HasPrefix(e.Name(), ".partial-") {
			_ = os.Remove(p)
			continue
		}
		items = append(items, item{p, info.Size(), info.ModTime().UnixNano()})
		total += info.Size()
	}
	if total <= budget {
		return
	}

	// Oldest first — "least recently pulled" is the best proxy we have for
	// "least likely to be wanted again".
	sort.Slice(items, func(i, j int) bool { return items[i].mod < items[j].mod })
	for _, it := range items {
		if total <= budget {
			return
		}
		if os.Remove(it.path) == nil {
			total -= it.size
		}
	}
}
