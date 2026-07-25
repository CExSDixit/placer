// Package device abstracts every interaction with the Android device.
//
// Everything the rest of adbfz does goes through the Device interface, so the
// TUI, indexer and transfer engine can be developed and tested against a
// fixture-backed fake with no phone attached. The real ADB implementation is
// deliberately thin: it is the only part that needs hardware to verify.
package device

import (
	"context"
	"time"
)

// Collection is a MediaStore content URI we know how to query.
type Collection string

const (
	Images    Collection = "content://media/external/images/media"
	Video     Collection = "content://media/external/video/media"
	Audio     Collection = "content://media/external/audio/media"
	Downloads Collection = "content://media/external/downloads"
	Files     Collection = "content://media/external/file"
)

// Kind is the coarse bucket a file falls into, used for tab grouping and to
// decide which preview tier applies.
type Kind int

const (
	KindOther Kind = iota
	KindImage
	KindVideo
	KindAudio
	KindDoc
)

func (k Kind) String() string {
	switch k {
	case KindImage:
		return "image"
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	case KindDoc:
		return "doc"
	}
	return "other"
}

// File is one row of the on-device index, normalised across collections.
type File struct {
	ID       string
	Path     string // _data; may be empty on locked-down builds (see ContentURI)
	Name     string // _display_name
	Size     int64
	Mime     string
	Bucket   string // bucket_display_name, i.e. the album/folder
	Taken    time.Time
	Added    time.Time
	Duration time.Duration // audio/video only
	Coll     Collection
}

// Kind classifies the file from its mime type, falling back to the collection
// it came from when the mime is missing or unhelpful.
func (f File) Kind() Kind {
	switch {
	case len(f.Mime) >= 6 && f.Mime[:6] == "image/":
		return KindImage
	case len(f.Mime) >= 6 && f.Mime[:6] == "video/":
		return KindVideo
	case len(f.Mime) >= 6 && f.Mime[:6] == "audio/":
		return KindAudio
	}
	switch f.Coll {
	case Images:
		return KindImage
	case Video:
		return KindVideo
	case Audio:
		return KindAudio
	}
	switch f.Mime {
	case "application/pdf", "text/plain", "text/markdown",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return KindDoc
	}
	return KindOther
}

// SortTime is the timestamp a human means by "when is this from": the capture
// time when MediaStore knows it, otherwise when it was added.
func (f File) SortTime() time.Time {
	if !f.Taken.IsZero() {
		return f.Taken
	}
	return f.Added
}

// ContentURI is the scoped-storage escape hatch for rows whose _data path is
// null or unreadable — the bytes can still be streamed by ID.
func (f File) ContentURI() string {
	if f.ID == "" {
		return ""
	}
	return string(f.Coll) + "/" + f.ID
}

// Query describes a single `content query` invocation.
type Query struct {
	Coll       Collection
	Projection []string
	Where      string
	Sort       string
}

// Progress reports transfer advancement for one file.
type Progress struct {
	Path    string
	Percent int
	Done    bool
	Err     error
}

// Device is the whole surface adbfz needs from a phone.
type Device interface {
	// Serial identifies the device, e.g. "23311JEGR07766".
	Serial() string

	// Query runs a MediaStore `content query` and returns one map per row,
	// keyed by projection column. Absent (NULL) columns are omitted.
	Query(ctx context.Context, q Query) ([]map[string]string, error)

	// ExecOut runs a shell command and returns raw stdout. It must be
	// binary-safe: implementations use `adb exec-out`, never `adb shell`,
	// because the latter mangles \n into \r\n and corrupts image data.
	ExecOut(ctx context.Context, cmd string) ([]byte, error)

	// Pull copies a remote file to a local path, reporting progress.
	Pull(ctx context.Context, remote, local string, prog func(Progress)) error
}
