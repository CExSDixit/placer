package preview

import (
	"os"
	"path/filepath"
	"testing"
)

// The real DNG sample is gitignored personal media (see
// testdata/sample/README in the repo root .gitignore), so this test skips
// cleanly on a fresh checkout instead of failing.
func sampleDNG(t *testing.T) []byte {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "sample")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("no testdata/sample; put real camera files (jpg/dng/mp4/wav) there to enable")
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".dng" {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Skip("no .dng file in testdata/sample")
	return nil
}

func TestExtractDNGPreview_RealSample(t *testing.T) {
	data := sampleDNG(t)
	jpg, err := ExtractDNGPreview(data)
	if err != nil {
		t.Fatalf("ExtractDNGPreview: %v", err)
	}
	if !isJPEG(jpg) {
		t.Fatalf("extracted preview does not start with a JPEG SOI marker")
	}
	if len(jpg) < 1000 {
		t.Fatalf("extracted preview looks too small to be real: %d bytes", len(jpg))
	}
	img, err := decodeStdlib("image/jpeg", jpg)
	if err != nil {
		t.Fatalf("decode extracted preview: %v", err)
	}
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		t.Fatalf("decoded preview has zero size")
	}
	t.Logf("embedded preview: %d bytes, %dx%d", len(jpg), b.Dx(), b.Dy())
}

func TestExtractDNGPreview_NotTIFF(t *testing.T) {
	if _, err := ExtractDNGPreview([]byte("not a tiff file at all")); err == nil {
		t.Fatal("expected an error for non-TIFF input")
	}
}

func TestExtractDNGPreview_Truncated(t *testing.T) {
	// A well-formed header with an IFD offset pointing past EOF must not
	// panic — this is the one place a subtly wrong offset would otherwise
	// crash the whole preview pipeline on cursor rest.
	data := []byte{'I', 'I', 42, 0, 100, 0, 0, 0}
	if _, err := ExtractDNGPreview(data); err == nil {
		t.Fatal("expected an error for a truncated IFD")
	}
}
