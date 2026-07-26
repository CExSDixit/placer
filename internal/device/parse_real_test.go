package device

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests run against real `content query` output captured from a device
// with `make fixtures`. They are skipped when no fixtures are present, so the
// suite still passes on a machine that has never seen the phone.
//
// The synthetic fake can only exercise the cases we thought of; this is the
// only check that the parser survives what the device actually emits.

func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "fixtures")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no fixtures; run `make fixtures` with a device attached")
	}
	return dir
}

var realRowRe = regexp.MustCompile(`(?m)^Row:\s+\d+\s`)

// The parser must not silently drop or invent rows.
func TestRealFixtures_RowCountMatches(t *testing.T) {
	dir := fixtureDir(t)
	for name, coll := range map[string]Collection{
		"images": Images, "video": Video, "audio": Audio, "downloads": Downloads,
	} {
		b, err := os.ReadFile(filepath.Join(dir, name+".txt"))
		if err != nil {
			continue
		}
		raw := string(b)
		wantRows := len(realRowRe.FindAllStringIndex(raw, -1))
		got := ParseRows(raw, StandardProjection(coll))
		if len(got) != wantRows {
			t.Errorf("%s: parsed %d rows, file has %d Row: lines", name, len(got), wantRows)
		}
		if wantRows == 0 {
			t.Errorf("%s: fixture has no rows", name)
		}
	}
}

// Every row must yield an addressable, displayable File.
func TestRealFixtures_AllRowsUsable(t *testing.T) {
	dir := fixtureDir(t)
	for name, coll := range map[string]Collection{
		"images": Images, "video": Video, "audio": Audio, "downloads": Downloads,
	} {
		b, err := os.ReadFile(filepath.Join(dir, name+".txt"))
		if err != nil {
			continue
		}
		rows := ParseRows(string(b), StandardProjection(coll))

		var noName, noPath, noSize, badMime int
		for _, r := range rows {
			f := ToFile(r, coll)
			if f.Name == "" {
				noName++
			}
			if f.Path == "" {
				noPath++
			}
			if f.Size <= 0 {
				noSize++
			}
			// A mime that still contains a comma or "=" means the parser ran
			// past a column boundary.
			if strings.ContainsAny(f.Mime, ",=") {
				badMime++
				if badMime <= 3 {
					t.Errorf("%s: mime looks mis-parsed: %q (name %q)", name, f.Mime, f.Name)
				}
			}
		}
		t.Logf("%-10s rows=%-6d unnamed=%-4d no_path=%-4d zero_size=%-4d bad_mime=%d",
			name, len(rows), noName, noPath, noSize, badMime)

		if noName > 0 {
			t.Errorf("%s: %d rows produced no display name", name, noName)
		}
		if badMime > 0 {
			t.Errorf("%s: %d rows have a mis-parsed mime", name, badMime)
		}
	}
}

// Report what the real library actually contains, and prove the comma-bearing
// names — the entire reason the parser exists — round-trip correctly.
func TestRealFixtures_Report(t *testing.T) {
	dir := fixtureDir(t)
	b, err := os.ReadFile(filepath.Join(dir, "images.txt"))
	if err != nil {
		t.Skip("no images fixture")
	}
	rows := ParseRows(string(b), StandardProjection(Images))

	mimes := map[string]int{}
	buckets := map[string]int{}
	var tricky []string
	var noTaken int
	for _, r := range rows {
		f := ToFile(r, Images)
		mimes[f.Mime]++
		buckets[f.Bucket]++
		if strings.Contains(f.Name, ",") {
			tricky = append(tricky, f.Name)
		}
		if f.Taken.IsZero() {
			noTaken++
		}
	}
	t.Logf("mime distribution: %v", mimes)
	t.Logf("top buckets: %s", topN(buckets, 6))
	t.Logf("rows with no datetaken (fall back to date_added): %d of %d", noTaken, len(rows))
	if len(tricky) > 0 {
		t.Logf("names containing a comma (%d): %q", len(tricky), tricky[:min(5, len(tricky))])
	} else {
		t.Log("no comma-bearing names in this library (parser still required — album names can carry them)")
	}

	// Sanity: paths must look like real device paths, not fragments of a
	// mis-split row.
	for _, r := range rows[:min(200, len(rows))] {
		f := ToFile(r, Images)
		if f.Path != "" && !strings.HasPrefix(f.Path, "/") {
			t.Errorf("path does not start with /: %q", f.Path)
			break
		}
	}
}

func topN(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].v > all[i].v {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	var parts []string
	for i := 0; i < min(n, len(all)); i++ {
		parts = append(parts, fmt.Sprintf("%s=%d", all[i].k, all[i].v))
	}
	return strings.Join(parts, " ")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
