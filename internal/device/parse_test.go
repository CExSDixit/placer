package device

import (
	"testing"
	"time"
)

func TestParseRows_Simple(t *testing.T) {
	out := `Row: 0 _data=/storage/emulated/0/DCIM/Camera/PXL_20260725_180427752.jpg, _display_name=PXL_20260725_180427752.jpg, _size=2102940
Row: 1 _data=/storage/emulated/0/DCIM/Camera/PXL_2.jpg, _display_name=PXL_2.jpg, _size=17`
	rows := ParseRows(out, []string{"_data", "_display_name", "_size"})
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if got := rows[0]["_size"]; got != "2102940" {
		t.Errorf("_size = %q", got)
	}
	if got := rows[0]["_display_name"]; got != "PXL_20260725_180427752.jpg" {
		t.Errorf("_display_name = %q", got)
	}
}

// The whole reason this parser exists: values containing ", " must not split.
func TestParseRows_CommaInValue(t *testing.T) {
	out := `Row: 0 _data=/sdcard/Pictures/Trip to Paris, France.jpg, _display_name=Trip to Paris, France.jpg, _size=42, bucket_display_name=Holiday, 2026`
	rows := ParseRows(out, []string{"_data", "_display_name", "_size", "bucket_display_name"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r["_display_name"] != "Trip to Paris, France.jpg" {
		t.Errorf("_display_name = %q", r["_display_name"])
	}
	if r["_data"] != "/sdcard/Pictures/Trip to Paris, France.jpg" {
		t.Errorf("_data = %q", r["_data"])
	}
	if r["_size"] != "42" {
		t.Errorf("_size = %q", r["_size"])
	}
	if r["bucket_display_name"] != "Holiday, 2026" {
		t.Errorf("bucket = %q", r["bucket_display_name"])
	}
}

func TestParseRows_EqualsAndCommaInValue(t *testing.T) {
	out := `Row: 0 _display_name=a=b, c.mp3, _size=7`
	rows := ParseRows(out, []string{"_display_name", "_size"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0]["_display_name"] != "a=b, c.mp3" {
		t.Errorf("_display_name = %q", rows[0]["_display_name"])
	}
	if rows[0]["_size"] != "7" {
		t.Errorf("_size = %q", rows[0]["_size"])
	}
}

// A value that literally contains ", <known-key>=" is the documented
// ambiguity. Assert the known behaviour so a future change is a conscious one.
func TestParseRows_PathologicalKeyLikeValue(t *testing.T) {
	out := `Row: 0 _display_name=foo, _size=1.jpg, _size=99`
	rows := ParseRows(out, []string{"_display_name", "_size"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	// The embedded ", _size=" reads as a boundary; last occurrence wins.
	if rows[0]["_size"] != "99" {
		t.Errorf("documented behaviour changed: _size = %q", rows[0]["_size"])
	}
}

func TestParseRows_NullColumnsOmitted(t *testing.T) {
	// content query omits NULL columns entirely rather than emitting empties.
	out := `Row: 0 _id=5, _size=12`
	rows := ParseRows(out, []string{"_id", "_data", "_display_name", "_size"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if _, ok := rows[0]["_data"]; ok {
		t.Errorf("_data should be absent, got %q", rows[0]["_data"])
	}
	if rows[0]["_id"] != "5" {
		t.Errorf("_id = %q", rows[0]["_id"])
	}
}

func TestParseRows_EmptyValue(t *testing.T) {
	out := `Row: 0 _display_name=, _size=3`
	rows := ParseRows(out, []string{"_display_name", "_size"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if v, ok := rows[0]["_display_name"]; !ok || v != "" {
		t.Errorf("want present-and-empty, got %q ok=%v", v, ok)
	}
}

func TestParseRows_NoiseLines(t *testing.T) {
	out := `No result found.
adb: warning: something
Row: 0 _size=1

`
	rows := ParseRows(out, []string{"_size"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
}

func TestParseRows_CRLF(t *testing.T) {
	out := "Row: 0 _size=1\r\nRow: 1 _size=2\r\n"
	rows := ParseRows(out, []string{"_size"})
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[1]["_size"] != "2" {
		t.Errorf("trailing \\r not stripped: %q", rows[1]["_size"])
	}
}

// A key that is a prefix of another must not shadow it.
func TestParseRows_PrefixKeys(t *testing.T) {
	out := `Row: 0 _id=1, _id_hash=abc, _size=2`
	rows := ParseRows(out, []string{"_id", "_id_hash", "_size"})
	if rows[0]["_id"] != "1" || rows[0]["_id_hash"] != "abc" {
		t.Errorf("prefix shadowing: _id=%q _id_hash=%q", rows[0]["_id"], rows[0]["_id_hash"])
	}
}

func TestParseRows_OutOfOrderColumns(t *testing.T) {
	out := `Row: 0 _size=9, _display_name=z.jpg, _id=3`
	rows := ParseRows(out, []string{"_id", "_display_name", "_size"})
	r := rows[0]
	if r["_id"] != "3" || r["_display_name"] != "z.jpg" || r["_size"] != "9" {
		t.Errorf("out-of-order parse failed: %#v", r)
	}
}

func TestToFile_Basics(t *testing.T) {
	row := map[string]string{
		"_id": "7", "_data": "/sdcard/DCIM/Camera/a.jpg", "_display_name": "a.jpg",
		"_size": "2102940", "mime_type": "image/jpeg",
		"datetaken": "1774461867000", "date_added": "1774461868",
		"bucket_display_name": "Camera",
	}
	f := ToFile(row, Images)
	if f.Size != 2102940 || f.Mime != "image/jpeg" || f.Kind() != KindImage {
		t.Errorf("bad file: %#v", f)
	}
	if f.Taken.IsZero() || f.SortTime() != f.Taken {
		t.Errorf("SortTime should prefer datetaken, got %v", f.SortTime())
	}
	if f.ContentURI() != string(Images)+"/7" {
		t.Errorf("ContentURI = %q", f.ContentURI())
	}
}

func TestToFile_JunkTimestamps(t *testing.T) {
	for _, ts := range []string{"0", "-1", "99999999999999", ""} {
		f := ToFile(map[string]string{"datetaken": ts, "date_added": "1774461868"}, Images)
		if !f.Taken.IsZero() {
			t.Errorf("datetaken=%q should be zero time, got %v", ts, f.Taken)
		}
		if f.SortTime().IsZero() {
			t.Errorf("should fall back to date_added for datetaken=%q", ts)
		}
	}
}

func TestToFile_NameFallbacks(t *testing.T) {
	// Audio rows often carry title but no _display_name.
	f := ToFile(map[string]string{"title": "Voice 003", "duration": "65000"}, Audio)
	if f.Name != "Voice 003" {
		t.Errorf("want title fallback, got %q", f.Name)
	}
	if f.Duration != 65*time.Second {
		t.Errorf("duration = %v", f.Duration)
	}
	if f.Kind() != KindAudio {
		t.Errorf("audio collection should imply KindAudio, got %v", f.Kind())
	}
	// Otherwise fall back to the basename of _data.
	f2 := ToFile(map[string]string{"_data": "/sdcard/Music/x, y.mp3"}, Audio)
	if f2.Name != "x, y.mp3" {
		t.Errorf("want basename fallback, got %q", f2.Name)
	}
	if f2.Bucket != "Music" {
		t.Errorf("want bucket from parent dir, got %q", f2.Bucket)
	}
}

func TestStandardProjection(t *testing.T) {
	if got := StandardProjection(Audio); !contains(got, "duration") || contains(got, "datetaken") {
		t.Errorf("audio projection wrong: %v", got)
	}
	if got := StandardProjection(Images); !contains(got, "datetaken") {
		t.Errorf("images projection missing datetaken: %v", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// The device emits the literal string "NULL" rather than omitting null
// columns — measured on a real Pixel 6a. Treating it as a value put the string
// "NULL" into mime types and made directory rows look like files.
func TestParseRows_LiteralNULLIsAbsent(t *testing.T) {
	out := `Row: 0 _id=1000053113, _data=/storage/emulated/0/Download/Nearby Share, _display_name=Nearby Share, _size=NULL, mime_type=NULL, date_added=NULL`
	rows := ParseRows(out, []string{"_id", "_data", "_display_name", "_size", "mime_type", "date_added"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	for _, k := range []string{"_size", "mime_type", "date_added"} {
		if v, ok := rows[0][k]; ok {
			t.Errorf("%s should be absent, got %q", k, v)
		}
	}
	if rows[0]["_display_name"] != "Nearby Share" {
		t.Errorf("display name = %q", rows[0]["_display_name"])
	}

	f := ToFile(rows[0], Downloads)
	if f.Mime != "" {
		t.Errorf("mime should be empty, got %q", f.Mime)
	}
	if f.Pullable() {
		t.Error("a directory row must not be Pullable")
	}
}

func TestParseRows_NullDatetakenFallsBack(t *testing.T) {
	// 5,688 real rows have datetaken=NULL and must fall back to date_added.
	out := `Row: 0 _data=/storage/emulated/0/Pictures/IMG-WA0006.jpeg, _display_name=IMG-WA0006.jpeg, _size=1000, mime_type=image/jpeg, datetaken=NULL, date_added=1774461868`
	rows := ParseRows(out, StandardProjection(Images))
	f := ToFile(rows[0], Images)
	if !f.Taken.IsZero() {
		t.Errorf("Taken should be zero for NULL datetaken, got %v", f.Taken)
	}
	if f.SortTime().IsZero() {
		t.Error("SortTime should fall back to date_added")
	}
	if !f.Pullable() {
		t.Error("a real file with a size and mime must be Pullable")
	}
}

func TestPullable(t *testing.T) {
	cases := []struct {
		name string
		f    File
		want bool
	}{
		{"real file", File{Path: "/sdcard/a.jpg", Mime: "image/jpeg", Size: 100}, true},
		{"directory row", File{Path: "/sdcard/Nearby Share"}, false},
		{"no path", File{Mime: "image/jpeg", Size: 100}, false},
		{"directory with inode size", File{Path: "/sdcard/CamScanner", Size: 3452}, false},
		{"mime but zero size", File{Path: "/sdcard/x.jpg", Mime: "image/jpeg"}, true},
	}
	for _, c := range cases {
		if got := c.f.Pullable(); got != c.want {
			t.Errorf("%s: Pullable() = %v, want %v", c.name, got, c.want)
		}
	}
}
