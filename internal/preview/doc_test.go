package preview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

// Doc-tier tests, over the 20 real device files staged in
// testdata/sample/docs (gitignored — see the phase 4 handoff for how to
// re-stage). Kept separate from TestEveryTierRendersThroughEveryProtocol
// because that test gates on HasFFmpeg(), which docs don't need.

func docsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "sample", "docs")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no testdata/sample/docs; see the phase 4 handoff for how to stage it")
	}
	return dir
}

func docFile(t *testing.T, dir, name, mime string) device.File {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Skipf("sample %s not present: %v", name, err)
	}
	return device.File{Path: name, Name: name, Size: fi.Size(), Mime: mime}
}

func docDev(t *testing.T, dir string) device.Device {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, ".cache"))
	return &device.Fake{SrcDir: dir, Latency: time.Millisecond, Speed: 100 << 20}
}

// TestDocTier_TextExtraction covers the ladder's non-image outcomes: PDF
// text, docx, xlsx, csv/ics text, a mht/legacy-doc metadata card, and the
// deliberately-binary (invalid).txt sniff failure. Every case must also
// clear the doc-tier invariant that matters most here: a text result must
// never carry an escape sequence, since nothing downstream of this test can
// tell a stray \x1b_G from a real graphics command.
func TestDocTier_TextExtraction(t *testing.T) {
	dir := docsDir(t)

	cases := []struct {
		file, mime string
		wantTier   Tier
		contains   string // substring expected in the joined Meta; empty to skip
	}{
		// Extracted with per-glyph spacing ("I N V O I C E") — a real PDF
		// text-extraction artifact, not a bug; assert on a run that survives it.
		{"AVIE967605A.PDF", "application/pdf", TierDoc, "Centretown Veterinary Hospital"},
		{"The Kubebuilder Book.pdf", "application/pdf", TierDoc, ""},
		{"water_bills_tax_reference.docx", mimeDocx, TierDoc, ""},
		// market_analysis-1.xlsx has no xl/sharedStrings.xml at all — every
		// string is inline in the worksheet — so this also pins that the
		// inline-string path alone produces a real table, not just a sheet
		// name.
		{"market_analysis-1.xlsx", mimeXlsx, TierDoc, "Role / Ti"},
		{"appointment.ics", "text/calendar", TierDoc, ""},
		{"mm_weighttracker_backup_2023_07_12.csv", "text/csv", TierDoc, ""},
		{"dtoken.epub", "application/epub+zip", TierDoc, ""},
		{"(invalid).txt", "text/plain", TierMeta, ""},
		{"Prompt Engineering _ Lil'Log.mht", "multipart/related", TierMeta, ""},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			dev := docDev(t, dir)
			f := docFile(t, dir, c.file, c.mime)

			res, err := Fetch(context.Background(), dev, f, 60, 20, ProtoHalfBlock)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if res.Tier != c.wantTier {
				t.Fatalf("tier = %v, want %v (note %q, meta %v)", res.Tier, c.wantTier, res.Note, res.Meta)
			}
			if res.HasImage() {
				t.Fatal("expected a text/meta result, got image bytes")
			}
			joined := strings.Join(res.Meta, "\n")
			if strings.ContainsRune(joined, '\x1b') {
				t.Error("doc text carries an escape sequence — graphics protocol leaked into a text result")
			}
			if c.contains != "" && !strings.Contains(joined, c.contains) {
				t.Errorf("Meta does not contain %q: %v", c.contains, res.Meta)
			}
		})
	}
}

// TestDocTier_XlsxMultiColumn pins the fix for a real gap: the old
// extraction only dumped a flat, order-joined shared-strings blob, which for
// this exact sample (no xl/sharedStrings.xml at all — every cell is inline)
// produced almost nothing. The new extraction must show several distinct
// columns from the same row, not just the first one.
func TestDocTier_XlsxMultiColumn(t *testing.T) {
	dir := docsDir(t)
	dev := docDev(t, dir)
	f := docFile(t, dir, "market_analysis-1.xlsx", mimeXlsx)

	res, err := Fetch(context.Background(), dev, f, 80, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	joined := strings.Join(res.Meta, "\n")
	for _, want := range []string{"Role / T", "Canada", "DevOps En"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Meta missing %q — columns beyond the first are not rendering: %v", want, res.Meta)
		}
	}
}

// TestDocTier_PDFAccumulatesPages pins the fix for a real gap: extraction
// used to stop at the FIRST page clearing the watermark threshold, so a
// document with a short first text-bearing page showed only that page even
// when later pages had much more. A multi-page book should now yield text
// that spans more than one page's worth.
func TestDocTier_PDFAccumulatesPages(t *testing.T) {
	dir := docsDir(t)
	data, err := os.ReadFile(filepath.Join(dir, "The Kubebuilder Book.pdf"))
	if err != nil {
		t.Skipf("sample not present: %v", err)
	}
	text, pages, ok := extractPDFText(data)
	if !ok {
		t.Fatal("extractPDFText: no text extracted")
	}
	if pages < 2 {
		t.Skip("sample is not multi-page; accumulation has nothing to prove here")
	}
	// A single page of this book runs well under 1000 runes; accumulating
	// several should clear that comfortably if more than one page's text
	// made it into the result.
	if n := len([]rune(text)); n < 1000 {
		t.Fatalf("accumulated text is only %d runes — looks like a single page, not several", n)
	}
}

// TestDocTier_ScannedPDFYieldsImage is the ladder's third rung: a scanned
// PDF has no extractable text, so it must fall through to the embedded-JPEG
// rasterisation and come back as an ordinary TierImage result — not a
// metadata card — through every rendering protocol, exactly like a photo.
// Both samples were confirmed scanned by measuring their extractable text
// with pdftotext: one page yields a single byte, the other 39 bytes of
// CamScanner watermark across 3 pages, both well under the ladder's
// minPDFTextRunes threshold.
func TestDocTier_ScannedPDFYieldsImage(t *testing.T) {
	dir := docsDir(t)
	scanned := []string{"245699913788760.pdf", "dogPassport_Lockey_05-16-2022.pdf"}
	protos := []Protocol{ProtoHalfBlock, ProtoQuadrant, ProtoKitty, ProtoIterm}

	for _, name := range scanned {
		for _, p := range protos {
			t.Run(name+"/"+p.String(), func(t *testing.T) {
				dev := docDev(t, dir)
				f := docFile(t, dir, name, "application/pdf")

				res, err := Fetch(context.Background(), dev, f, 40, 20, p)
				if err != nil {
					t.Fatalf("Fetch: %v", err)
				}
				if res.Tier != TierImage {
					t.Fatalf("tier = %v, want TierImage (note %q)", res.Tier, res.Note)
				}
				if !res.HasImage() {
					t.Fatal("HasImage() = false for a scanned-PDF image result")
				}
				if len(res.Rendered) == 0 {
					t.Fatal("no rendered bytes")
				}
			})
		}
	}
}

// TestDocTier_TextSizeGuard exercises the >2MB head-pull path: Size is
// overridden past the guard on a small real fixture, so ddHead runs against
// real bytes (Fake's dd path reads the same on-disk file regardless of the
// File.Size the test claims) without needing a >2MB fixture checked in.
func TestDocTier_TextSizeGuard(t *testing.T) {
	dir := docsDir(t)
	dev := docDev(t, dir)

	name := "mm_weighttracker_backup_2023_07_12.csv"
	f := docFile(t, dir, name, "text/csv")
	f.Size = docTextMaxBytes + 1

	res, err := Fetch(context.Background(), dev, f, 60, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierDoc {
		t.Fatalf("tier = %v, want TierDoc (note %q)", res.Tier, res.Note)
	}
}

// TestDocTier_Caching pins the same regression phase 3 hit for images: the
// second fetch must hit the cache and return byte-identical Meta.
func TestDocTier_Caching(t *testing.T) {
	dir := docsDir(t)
	dev := docDev(t, dir)
	f := docFile(t, dir, "AVIE967605A.PDF", "application/pdf")

	res1, err := Fetch(context.Background(), dev, f, 60, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	res2, err := Fetch(context.Background(), dev, f, 60, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if strings.Join(res1.Meta, "\n") != strings.Join(res2.Meta, "\n") {
		t.Fatal("cached doc text differs from first extraction")
	}
}

// TestDocKind confirms the mime map routes the staged sample set (plus a
// couple of long-tail mimes only seen in the full-device measurement) to
// KindDoc, not KindOther — the fetch path is unreachable otherwise.
func TestDocKind(t *testing.T) {
	mimes := []string{
		"application/pdf",
		mimeDocx, mimeXlsx, mimePptx, mimeDoc,
		"application/epub+zip", "application/zip",
		"application/vnd.android.package-archive",
		"application/vnd.apple.pkpass",
		"application/x-cbz", "application/vnd.comicbook+zip",
		mimeMht1, mimeMht2,
		"text/plain", "text/markdown", "text/csv", "text/calendar", "text/html",
		"application/json",
	}
	for _, m := range mimes {
		f := device.File{Mime: m}
		if got := f.Kind(); got != device.KindDoc {
			t.Errorf("Kind(%q) = %v, want KindDoc", m, got)
		}
	}
}
