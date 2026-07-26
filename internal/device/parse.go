package device

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// `content query` emits one line per row:
//
//	Row: 0 _data=/sdcard/DCIM/Camera/PXL_1.jpg, _display_name=PXL_1.jpg, _size=2102940
//
// This output is NOT safely comma-splittable. Display names, album names and
// artist tags routinely contain ", " — splitting on bare commas silently
// produces wrong rows rather than an error, which is the worst possible
// failure mode for a tool that then transfers files.
//
// So we never split on commas. We locate the *known projection keys* at
// `key=` positions anchored to either the start of the row or a preceding
// ", ", and take each value as everything up to the next such anchor. Values
// may then contain commas, equals signs, or anything else.
//
// Residual ambiguity: a value containing a literal ", <projected-key>=" is
// indistinguishable from a column boundary. That is pathological (it requires
// a filename like `foo, _size=1.jpg`) and is accepted as a known limit.

var rowPrefix = regexp.MustCompile(`^Row:\s+\d+\s+`)

// buildBoundaryRe compiles the anchor pattern for one projection. Keys are
// sorted longest-first so that a key which is a prefix of another (_id vs
// _id_hash) cannot shadow it.
func buildBoundaryRe(projection []string) *regexp.Regexp {
	keys := make([]string, len(projection))
	copy(keys, projection)
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	alt := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		alt = append(alt, regexp.QuoteMeta(k))
	}
	if len(alt) == 0 {
		return nil
	}
	return regexp.MustCompile(`(?:^|, )(` + strings.Join(alt, "|") + `)=`)
}

// ParseRows turns raw `content query` stdout into one map per row, keyed by
// projection column. Columns the device reported as NULL are omitted from the
// map, whether it omitted them or emitted the literal string "NULL".
//
// Caveat: a value that is genuinely the four characters "NULL" is
// indistinguishable from a null column and will be dropped.
func ParseRows(out string, projection []string) []map[string]string {
	re := buildBoundaryRe(projection)
	if re == nil {
		return nil
	}
	var rows []map[string]string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if !rowPrefix.MatchString(line) {
			continue // "No result found.", blank lines, adb warnings
		}
		body := rowPrefix.ReplaceAllString(line, "")

		locs := re.FindAllStringSubmatchIndex(body, -1)
		if len(locs) == 0 {
			continue
		}
		row := make(map[string]string, len(locs))
		for i, m := range locs {
			key := body[m[2]:m[3]]
			valStart := m[1] // just past "key="
			valEnd := len(body)
			if i+1 < len(locs) {
				valEnd = locs[i+1][0] // start of next anchor, incl. its ", "
			}
			val := body[valStart:valEnd]
			// The device emits the literal string "NULL" for null columns
			// rather than omitting them (measured: 5,688 datetaken=NULL and 10
			// mime_type=NULL rows on a real Pixel 6a). Treat it as absent, so
			// downstream code never sees "NULL" as a mime type or filename.
			if val == "NULL" {
				continue
			}
			row[key] = val
		}
		rows = append(rows, row)
	}
	return rows
}

// StandardProjection is the column set we ask for per collection. Audio and
// video carry duration; only media carries datetaken.
func StandardProjection(c Collection) []string {
	base := []string{"_id", "_data", "_display_name", "_size", "mime_type", "date_added"}
	switch c {
	case Images:
		return append(base, "datetaken", "bucket_display_name")
	case Video:
		return append(base, "datetaken", "bucket_display_name", "duration")
	case Audio:
		return append(base, "duration", "title", "artist", "album")
	default:
		return base
	}
}

func atoi64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// epochMillis and epochSeconds tolerate the junk MediaStore sometimes holds:
// zero, negative, and absurdly out-of-range values all become the zero time
// rather than a date in 1970 or the year 50000.
func epochMillis(s string) time.Time {
	n := atoi64(s)
	if n <= 0 || n > 4102444800000 { // > year 2100
		return time.Time{}
	}
	return time.UnixMilli(n)
}

func epochSeconds(s string) time.Time {
	n := atoi64(s)
	if n <= 0 || n > 4102444800 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// ToFile normalises one parsed row into a File.
func ToFile(row map[string]string, c Collection) File {
	f := File{
		ID:     row["_id"],
		Path:   row["_data"],
		Name:   row["_display_name"],
		Size:   atoi64(row["_size"]),
		Mime:   row["mime_type"],
		Bucket: row["bucket_display_name"],
		Taken:  epochMillis(row["datetaken"]),
		Added:  epochSeconds(row["date_added"]),
		Coll:   c,
	}
	if d := atoi64(row["duration"]); d > 0 {
		f.Duration = time.Duration(d) * time.Millisecond
	}
	// Audio rows often lack _display_name but carry a title; and any row can
	// fall back to the basename of its path.
	if f.Name == "" {
		if t := row["title"]; t != "" {
			f.Name = t
		} else if f.Path != "" {
			if i := strings.LastIndex(f.Path, "/"); i >= 0 {
				f.Name = f.Path[i+1:]
			} else {
				f.Name = f.Path
			}
		}
	}
	if f.Bucket == "" && f.Path != "" {
		if i := strings.LastIndex(f.Path, "/"); i > 0 {
			dir := f.Path[:i]
			if j := strings.LastIndex(dir, "/"); j >= 0 {
				f.Bucket = dir[j+1:]
			}
		}
	}
	return f
}
