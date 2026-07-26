// Package index loads the device's file index once and serves all filtering
// locally, so fuzzy search never touches the phone.
package index

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/sahilm/fuzzy"
)

// Tab groups files the way the UI presents them.
type Tab int

const (
	TabPhotos Tab = iota
	TabVideo
	TabAudio
	TabDocs
	TabAll
	numTabs
)

var TabNames = [numTabs]string{"Photos", "Video", "Audio", "Docs", "All"}

func (t Tab) String() string {
	if t >= 0 && int(t) < len(TabNames) {
		return TabNames[t]
	}
	return "?"
}

// SortMode controls list ordering.
type SortMode int

const (
	SortDate SortMode = iota // newest first
	SortName
	SortSize // largest first
)

var SortNames = map[SortMode]string{SortDate: "date", SortName: "name", SortSize: "size"}

// Index is the in-memory library.
type Index struct {
	All  []device.File
	tabs [numTabs][]device.File
}

// Load queries every collection concurrently and merges the results,
// de-duplicating by device path (the `file` collection overlaps the media
// ones). A collection that fails is skipped rather than failing the load —
// some are absent on older Android versions.
func Load(ctx context.Context, dev device.Device) (*Index, []error) {
	colls := []device.Collection{device.Images, device.Video, device.Audio, device.Downloads}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		out  []device.File
		errs []error
	)
	for _, c := range colls {
		wg.Add(1)
		go func(c device.Collection) {
			defer wg.Done()
			proj := device.StandardProjection(c)
			sortKey := "date_added DESC"
			if c == device.Images || c == device.Video {
				sortKey = "datetaken DESC"
			}
			rows, err := dev.Query(ctx, device.Query{Coll: c, Projection: proj, Sort: sortKey})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for _, r := range rows {
				out = append(out, device.ToFile(r, c))
			}
		}(c)
	}
	wg.Wait()

	ix := &Index{}
	seen := make(map[string]struct{}, len(out))
	for _, f := range out {
		if f.Path != "" {
			if _, dup := seen[f.Path]; dup {
				continue
			}
			seen[f.Path] = struct{}{}
		}
		if f.Name == "" && f.Path == "" {
			continue // nothing addressable
		}
		if !f.Pullable() {
			continue // directory row, not a file
		}
		ix.All = append(ix.All, f)
	}
	ix.rebuild()
	return ix, errs
}

// NewFrom builds an index from files directly (used by tests).
func NewFrom(files []device.File) *Index {
	ix := &Index{All: files}
	ix.rebuild()
	return ix
}

func (ix *Index) rebuild() {
	for i := range ix.tabs {
		ix.tabs[i] = nil
	}
	for _, f := range ix.All {
		switch f.Kind() {
		case device.KindImage:
			ix.tabs[TabPhotos] = append(ix.tabs[TabPhotos], f)
		case device.KindVideo:
			ix.tabs[TabVideo] = append(ix.tabs[TabVideo], f)
		case device.KindAudio:
			ix.tabs[TabAudio] = append(ix.tabs[TabAudio], f)
		default:
			ix.tabs[TabDocs] = append(ix.tabs[TabDocs], f)
		}
	}
	ix.tabs[TabAll] = ix.All
}

func (ix *Index) Tab(t Tab) []device.File {
	if t < 0 || int(t) >= len(ix.tabs) {
		return nil
	}
	return ix.tabs[t]
}

func (ix *Index) Counts() map[Tab]int {
	m := make(map[Tab]int, numTabs)
	for i := range ix.tabs {
		m[Tab(i)] = len(ix.tabs[i])
	}
	return m
}

// haystack is what fuzzy search matches against: name, album/folder and mime,
// so "camera jpeg" or "screenshots" both work.
func haystack(f device.File) string {
	return f.Name + " " + f.Bucket + " " + f.Mime
}

// BucketCount is one row of a bucket/album breakdown.
type BucketCount struct {
	Name  string
	Count int
}

// Buckets reports every distinct bucket_display_name in tab t, largest
// first. bucket_display_name is just the leaf directory name MediaStore
// found the file in — it has nothing to do with which app "owns" a photo,
// so two unrelated folders that happen to share (or nearly share) a name
// show up as two separate, unrelated buckets here. That's real, not a bug:
// e.g. "WhatsApp Images" is WhatsApp's own app-scoped media directory, while
// a same-ish-named "WhatsApp" or "Whatsapp" under Pictures/ is typically a
// folder some other app or share action created to mirror the source name.
func (ix *Index) Buckets(t Tab) []BucketCount {
	counts := map[string]int{}
	for _, f := range ix.Tab(t) {
		name := f.Bucket
		if name == "" {
			name = "(none)"
		}
		counts[name]++
	}
	out := make([]BucketCount, 0, len(counts))
	for name, n := range counts {
		out = append(out, BucketCount{Name: name, Count: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// View is an ordered, filtered slice of the index ready for rendering.
type View struct {
	Files   []device.File
	Matches [][]int // per-file matched byte offsets in Name, for highlighting
}

// Build applies tab, bucket, query and sort. An empty query keeps the sorted
// order; a non-empty query orders by fuzzy score, which is what makes typing
// feel responsive. bucket, when non-empty, restricts to files whose
// bucket_display_name matches exactly (case-insensitive) — the album/folder
// filter real usage demands: WhatsApp Images (5,167 files) dwarfs Camera
// (2,186), so "filter to Camera" is the actual bulk-curation workflow, not
// an edge case.
func (ix *Index) Build(t Tab, bucket, query string, mode SortMode) View {
	src := ix.Tab(t)
	if bucket != "" {
		filtered := make([]device.File, 0, len(src))
		for _, f := range src {
			if strings.EqualFold(f.Bucket, bucket) {
				filtered = append(filtered, f)
			}
		}
		src = filtered
	}

	if query == "" {
		files := make([]device.File, len(src))
		copy(files, src)
		sortFiles(files, mode)
		return View{Files: files, Matches: make([][]int, len(files))}
	}

	hs := make([]string, len(src))
	for i, f := range src {
		hs[i] = haystack(f)
	}
	res := fuzzy.Find(query, hs)

	v := View{Files: make([]device.File, 0, len(res)), Matches: make([][]int, 0, len(res))}
	for _, m := range res {
		f := src[m.Index]
		v.Files = append(v.Files, f)
		// Keep only the offsets that land inside the name portion.
		var inName []int
		for _, ix := range m.MatchedIndexes {
			if ix < len(f.Name) {
				inName = append(inName, ix)
			}
		}
		v.Matches = append(v.Matches, inName)
	}
	return v
}

func sortFiles(files []device.File, mode SortMode) {
	switch mode {
	case SortName:
		sort.SliceStable(files, func(i, j int) bool {
			return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
		})
	case SortSize:
		sort.SliceStable(files, func(i, j int) bool { return files[i].Size > files[j].Size })
	default:
		sort.SliceStable(files, func(i, j int) bool {
			return files[i].SortTime().After(files[j].SortTime())
		})
	}
}
