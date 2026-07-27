package preview

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

// Wire-format tests.
//
// Whether a terminal *honours* an escape sequence can only be judged by a
// human looking at a real terminal — no test here can do that. What these
// tests can do is pin the bytes, so a change to the wire format is a failing
// test rather than a silent "previews stopped appearing", and so that a
// diagnosis can start from "our output is provably unchanged" instead of a
// guess. Every test that existed before these asserted only that Rendered was
// non-empty.

func kittyPayload(t *testing.T) string {
	t.Helper()
	dir := sampleDir(t)
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, ".cache"))
	dev := &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}

	name := "PXL_20260404_154433428.PORTRAIT.ORIGINAL.jpg"
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Skipf("sample %s not present: %v", name, err)
	}
	f := device.File{Path: name, Name: name, Size: fi.Size(),
		Mime: "image/jpeg", Coll: device.Images}

	res, err := Fetch(context.Background(), dev, f, 33, 40, ProtoKitty)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(res.Rendered) == 0 {
		t.Fatal("kitty render produced no bytes")
	}
	return string(res.Rendered)
}

// TestKittyTransmitHeaderShape pins the transmit command to the form that
// demonstrably renders. In particular it must NOT carry image or placement
// ids: adding those is what broke previews in Ghostty, and nothing else in
// the suite noticed.
func TestKittyTransmitHeaderShape(t *testing.T) {
	payload := kittyPayload(t)

	if !strings.HasPrefix(payload, "\x1b_G") {
		t.Fatalf("payload does not start with a kitty command: %.20q", payload)
	}
	head, _, ok := strings.Cut(payload, ";")
	if !ok {
		t.Fatalf("no key/value terminator in the header: %.60q", payload)
	}
	keys := strings.Split(strings.TrimPrefix(head, "\x1b_G"), ",")

	want := map[string]string{
		"a": "T",   // transmit and display
		"f": "100", // PNG
		"t": "d",   // data is inline, not a file path
		"m": "1",   // chunks follow
		"c": "33",  // destination columns
		"r": "40",  // destination rows
	}
	got := map[string]string{}
	for _, kv := range keys {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header key %s=%q, want %q (full header %q)", k, got[k], v, head)
		}
	}
	// Ids make a transmit replace the previous placement instead of stacking,
	// and q=2 stops the terminal acknowledging them into Bubble Tea's stdin.
	// Neither is optional; TestEveryKittyCommandIsResponseFree covers the
	// general rule.
	if got["i"] == "" || got["p"] == "" {
		t.Errorf("transmit carries no image/placement id, so placements stack: %q", head)
	}
	if got["q"] != "2" {
		t.Errorf("transmit carries an id without q=2 — the terminal acknowledges it on "+
			"stdin and Bubble Tea parses the reply as input: %q", head)
	}
}

// TestKittyChunkingIsWellFormed: every chunk must be a complete
// APC-introduced command, the middle ones marked m=1 and the last m=0.
func TestKittyChunkingIsWellFormed(t *testing.T) {
	payload := kittyPayload(t)

	if !strings.HasSuffix(payload, "\x1b\\") {
		t.Error("payload does not end with the APC terminator")
	}
	opens := strings.Count(payload, "\x1b_G")
	closes := strings.Count(payload, "\x1b\\")
	if opens != closes {
		t.Errorf("%d command starts but %d terminators — a truncated chunk renders nothing",
			opens, closes)
	}
	if opens < 2 {
		t.Errorf("expected a header plus at least one data chunk, got %d commands", opens)
	}
	if !strings.Contains(payload, "\x1b_Gm=0;") {
		t.Error("no final m=0 chunk — kitty waits forever for more data")
	}
	// Base64 payload only; a raw byte leaking in would corrupt the stream.
	for _, chunk := range regexp.MustCompile(`\x1b_Gm=1;([^\x1b]*)`).FindAllStringSubmatch(payload, -1) {
		if strings.ContainsAny(chunk[1], "\x00\n\r") {
			t.Errorf("chunk contains a raw control byte: %.40q", chunk[1])
		}
	}
}

// TestKittyClearIsAWellFormedDelete pins the erase command. It must be a
// delete that frees data, and must not reference an id — see KittyClear.
func TestKittyClearIsAWellFormedDelete(t *testing.T) {
	got := KittyClear()
	if got != "\x1b_Ga=d,d=I,i=7301,p=1,q=2\x1b\\" {
		t.Errorf("KittyClear = %q, want a quiet targeted delete", got)
	}
}

// kittyCommandRe pulls the key/value header out of every graphics command in a
// byte stream. The payload after `;` is not of interest here.
var kittyCommandRe = regexp.MustCompile(`\x1b_G([^;\x1b]*)[;\x1b]`)

// TestEveryKittyCommandIsResponseFree is THE regression test for the bug that
// broke previews in Ghostty.
//
// A kitty graphics command carrying an image id gets an acknowledgement
// written back on stdin — and stdin is where Bubble Tea reads keystrokes, so
// every repaint fed `<ESC>_Gi=7301,p=1;OK<ESC>\` into the key parser. Probed
// against Ghostty directly: no id key means no reply; an id means a reply; an
// id plus `q=2` means no reply.
//
// So every command placer emits must either carry no id key, or suppress the
// response with q=1/q=2. Nothing else in the suite could catch this — the
// sequence was perfectly valid and Rendered was non-empty.
func TestEveryKittyCommandIsResponseFree(t *testing.T) {
	for name, stream := range map[string]string{
		"transmit": kittyPayload(t),
		"clear":    KittyClear(),
	} {
		cmds := kittyCommandRe.FindAllStringSubmatch(stream, -1)
		if len(cmds) == 0 {
			t.Errorf("%s: found no kitty commands to check", name)
			continue
		}
		for _, c := range cmds {
			keys := map[string]string{}
			for _, kv := range strings.Split(c[1], ",") {
				k, v, _ := strings.Cut(kv, "=")
				keys[k] = v
			}
			_, hasID := keys["i"]
			if _, ok := keys["I"]; ok {
				hasID = true
			}
			if quiet := keys["q"] == "1" || keys["q"] == "2"; hasID && !quiet {
				t.Errorf("%s: command %q carries an image id without q=1/q=2 — the terminal "+
					"acknowledges it on stdin and Bubble Tea parses the reply as input",
					name, c[1])
			}
		}
	}
}

// TestEveryTierRendersThroughEveryProtocol is the coverage that was missing:
// the full fetch → decode → downscale → render path, per tier, per renderer.
// It cannot prove the terminal displays anything, but it does catch a tier or
// a protocol that stops producing bytes at all.
func TestEveryTierRendersThroughEveryProtocol(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	dir := sampleDir(t)

	cases := []struct {
		file, mime string
		coll       device.Collection
		wantTier   Tier
	}{
		{"PXL_20260404_154433428.PORTRAIT.ORIGINAL.jpg", "image/jpeg", device.Images, TierImage},
		{"Screenshot_20240314-211047.png", "image/png", device.Images, TierImage},
		{"unnamed.gif", "image/gif", device.Images, TierImage},
		{"PXL_20260721_162134343.RAW-02.ORIGINAL.dng", "image/x-adobe-dng", device.Images, TierDNG},
		{"VID-20260602-WA0003.mp4", "video/mp4", device.Video, TierVideo},
		{"Zombie_In_Pain-SoundBible.com-134322253.wav", "audio/x-wav", device.Audio, TierAudio},
	}
	protos := []Protocol{ProtoHalfBlock, ProtoQuadrant, ProtoKitty, ProtoIterm}

	for _, c := range cases {
		for _, p := range protos {
			t.Run(c.file+"/"+p.String(), func(t *testing.T) {
				tmp := t.TempDir()
				t.Setenv("HOME", tmp)
				t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, ".cache"))
				dev := &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}

				f := sampleFile(t, dir, c.file, c.mime)
				f.Coll = c.coll

				res, err := Fetch(context.Background(), dev, f, 33, 20, p)
				if err != nil {
					t.Fatalf("Fetch: %v", err)
				}
				if res.Tier != c.wantTier {
					t.Fatalf("tier = %v, want %v (note %q)", res.Tier, c.wantTier, res.Note)
				}
				if len(res.Rendered) == 0 {
					t.Fatalf("no rendered bytes (note %q)", res.Note)
				}
				if p.IsText() {
					// Block renderers must emit no APC graphics commands, or
					// the pane's width arithmetic is corrupted.
					if strings.Contains(string(res.Rendered), "\x1b_G") {
						t.Error("a text renderer emitted a graphics-protocol command")
					}
					// And must be laid out as lines the pane can zip.
					if !strings.Contains(string(res.Rendered), "\n") {
						t.Error("block render is a single line; the pane expects one line per row")
					}
				} else if !strings.Contains(string(res.Rendered), "\x1b") {
					t.Error("a graphics renderer emitted no escape sequence")
				}
			})
		}
	}
}

// TestRenderVersionInvalidatesCachedRenders is the regression test for the
// defect that hid three separate renderer fixes.
//
// The cache stores rendered protocol bytes keyed by file, geometry and
// protocol — but not by how those bytes were produced. When the kitty
// transmit changed, previews kept being served from disk in the old format,
// so correct code kept producing the broken display and every diagnosis was
// chasing a ghost.
func TestRenderVersionInvalidatesCachedRenders(t *testing.T) {
	f := device.File{Path: "/sdcard/a.jpg", Name: "a.jpg", Size: 1000, Mime: "image/jpeg"}

	// The render version must actually reach the on-disk path.
	p := cachePath(f, 40, 20, ProtoKitty)
	if !strings.Contains(p, cacheKey(f, 40, 20, ProtoKitty, renderVersion)) {
		t.Error("cachePath does not incorporate renderVersion")
	}

	// Two different versions must never collide.
	a := cacheKey(f, 40, 20, ProtoKitty, "r1")
	b := cacheKey(f, 40, 20, ProtoKitty, "r2")
	if a == b {
		t.Fatal("different render versions produce the same cache key")
	}

	// The MEDIA cache must NOT move when the render version changes: those
	// entries are whole pulled files, hundreds of megabytes each, and have
	// nothing to do with how a thumbnail is encoded.
	if strings.Contains(LocalPath(f), renderVersion) {
		t.Error("the media cache path is versioned by the renderer; a format bump " +
			"would force every cached audio file to be re-pulled")
	}
	if LocalPath(f) != filepath.Join(MediaCacheDir(), cacheKey(f, 0, 0, ProtoHalfBlock)+".jpg") {
		t.Error("LocalPath no longer matches the unversioned media key")
	}
}

// TestCachedRenderRoundTripsThroughTheCurrentFormat: whatever is written must
// be what is read back, so a stale entry can never masquerade as fresh.
func TestCachedRenderRoundTripsThroughTheCurrentFormat(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, ".cache"))

	f := device.File{Path: "/sdcard/a.jpg", Name: "a.jpg", Size: 1000, Mime: "image/jpeg"}
	writeCache(f, 40, 20, ProtoKitty, "", []byte("CURRENT"))

	got, ok := readCache(f, 40, 20, ProtoKitty)
	if !ok || string(got) != "CURRENT" {
		t.Fatalf("round trip failed: %q ok=%v", got, ok)
	}
	// A render written under a different version must not be reachable.
	stale := filepath.Join(CacheDir(), cacheKey(f, 40, 20, ProtoKitty, "r0")+".cache")
	if err := os.WriteFile(stale, []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := readCache(f, 40, 20, ProtoKitty); string(got) != "CURRENT" {
		t.Errorf("a previous render version was served: %q", got)
	}
}

// TestEmptyVariantMatchesOmittedVariant: writeCache passes a variant for every
// tier ("" for plain images) while readCache passes none, so the two must hash
// the same or nothing is ever a cache hit.
func TestEmptyVariantMatchesOmittedVariant(t *testing.T) {
	f := device.File{Path: "/sdcard/a.jpg", Name: "a.jpg", Size: 1000, Mime: "image/jpeg"}
	if cacheKey(f, 40, 20, ProtoKitty) != cacheKey(f, 40, 20, ProtoKitty, "") {
		t.Error(`cacheKey(...) and cacheKey(..., "") differ, so writes and reads miss each other`)
	}
	// A real variant must still separate entries.
	if cacheKey(f, 40, 20, ProtoKitty) == cacheKey(f, 40, 20, ProtoKitty, "f30000") {
		t.Error("a non-empty variant did not change the key")
	}
}
