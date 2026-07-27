package preview

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/ledongthuc/pdf"
)

// The Docs tab measured 357 rows on the reference device, 75% of them PDFs.
// The ladder below covers that in order: PDF (text, then an embedded-scan
// image, then a metadata card), plain text (mime-routed, printability-
// sniffed), docx/xlsx, and a shared zip-family entry listing as stretch.
// Everything here is TierDoc/TierMeta — the graphics/kitty transmit path
// phase 3 hardened is untouched. A scanned PDF is the one exception that
// produces an image: it goes through fetchImage's own decode/downscale/
// render chain (RenderImage) and comes back as TierImage, exactly like any
// other photo.

const (
	docTextVariant  = "doc-text"
	docTextMaxBytes = 2 << 20 // 2 MiB — above this, pull only a head chunk
	docHeadMiB      = 1       // ~1 MiB head pull for oversized text files
	maxDocLines     = 400     // bounds cache size and render cost; plenty for a preview
	minPDFTextRunes = 40      // below this a "text" page is really just a watermark
	pdfTextPages    = 3       // cover pages are often image-only; check a few
)

var (
	mimeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimePptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mimeDoc  = "application/msword"
	mimeMht1 = "multipart/related"
	mimeMht2 = "message/rfc822"
)

var zipFamilyMimes = map[string]bool{
	"application/epub+zip":                    true,
	"application/zip":                         true,
	"application/vnd.android.package-archive": true,
	"application/vnd.apple.pkpass":            true,
	"application/x-cbz":                       true,
	"application/vnd.comicbook+zip":           true,
}

func fetchDoc(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	switch {
	case mimeIs(f.Mime, "application/pdf"):
		return fetchPDF(ctx, dev, f, cellW, cellH, proto)
	case f.Mime == mimeDocx:
		return fetchDocx(ctx, dev, f, cellW, cellH, proto)
	case f.Mime == mimeXlsx:
		return fetchXlsx(ctx, dev, f, cellW, cellH, proto)
	case f.Mime == mimeDoc:
		return metaResult(f, "doc — legacy binary format, metadata only"), nil
	case f.Mime == mimePptx:
		return metaResult(f, "pptx — metadata only"), nil
	case f.Mime == mimeMht1 || f.Mime == mimeMht2:
		return metaResult(f, "mht — metadata only"), nil
	case zipFamilyMimes[f.Mime]:
		return fetchZipListing(ctx, dev, f, cellW, cellH, proto)
	case mimeIs(f.Mime, "text/"), f.Mime == "application/json":
		return fetchText(ctx, dev, f, cellW, cellH, proto)
	default:
		return metaResult(f, "no preview for this file type"), nil
	}
}

// splitLines turns a cached (or freshly wrapped) newline-joined blob back
// into the []string Meta wants.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func docTextResult(f device.File, cellW, cellH int, proto Protocol, lines []string) Result {
	if len(lines) > maxDocLines {
		lines = lines[:maxDocLines]
	}
	writeCache(f, cellW, cellH, proto, docTextVariant, []byte(strings.Join(lines, "\n")))
	return Result{Tier: TierDoc, Meta: lines}
}

// cleanText strips control characters (PDF/docx extraction leaves stray
// \x1f and similar) and collapses runs of whitespace, which extracted text
// is full of ("P atient", double spaces from kerning) but real prose is not.
// Newlines are kept as paragraph breaks; wrapText re-flows within them.
func cleanText(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune('\n')
			lastSpace = false
		case r == utf8.RuneError, r < 0x20:
			// drop control chars and decode errors outright
		case unicode.IsSpace(r):
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
		default:
			b.WriteRune(r)
			lastSpace = false
		}
	}
	return b.String()
}

// wrapText re-flows cleaned text to width, one output line per input line
// (paragraph), so the pane shows readable prose rather than lines truncated
// mid-word at pw.
func wrapText(s string, width int) []string {
	if width < 10 {
		width = 10
	}
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		line := ""
		for _, w := range strings.Fields(para) {
			switch {
			case line == "":
				line = w
			case len(line)+1+len(w) <= width:
				line += " " + w
			default:
				lines = append(lines, line)
				line = w
			}
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// --- PDF ---------------------------------------------------------------

func fetchPDF(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	if cached, ok := readCache(f, cellW, cellH, proto, docTextVariant); ok {
		return Result{Tier: TierDoc, Meta: splitLines(string(cached))}, nil
	}
	if cached, ok := readCache(f, cellW, cellH, proto); ok {
		return Result{Tier: TierImage, Rendered: cached}, nil
	}

	local, err := EnsureLocal(ctx, dev, f)
	if err != nil {
		return metaResult(f, "pull failed: "+err.Error()), nil
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	data, err := os.ReadFile(local)
	if err != nil {
		return metaResult(f, "read failed: "+err.Error()), nil
	}

	if text, pages, ok := extractPDFText(data); ok {
		return docTextResult(f, cellW, cellH, proto, wrapText(text, cellW)), nil
	} else if img, ok := extractEmbeddedJPEG(data); ok {
		small := downscale(img, cellW, cellH, proto)
		rendered, err := Render(small, proto, cellW, cellH)
		if err == nil {
			writeCache(f, cellW, cellH, proto, "", rendered)
			return Result{Tier: TierImage, Rendered: rendered}, nil
		}
		return metaResult(f, fmt.Sprintf("pdf — %d page(s), render failed", pages)), nil
	} else {
		return metaResult(f, fmt.Sprintf("pdf — %d page(s), no extractable text", pages)), nil
	}
}

// extractPDFText scans the first few pages for the first one carrying real
// text (cover pages are often image-only). The rsc.io/pdf lineage panics on
// malformed input rather than returning an error, so this is wrapped in a
// recover — a corrupt PDF must fall through to the image/metadata rungs of
// the ladder, never crash the fetch.
func extractPDFText(data []byte) (text string, pages int, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	rd, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", 0, false
	}
	pages = rd.NumPage()
	limit := pages
	if limit > pdfTextPages {
		limit = pdfTextPages
	}
	fonts := map[string]*pdf.Font{}
	for i := 1; i <= limit; i++ {
		p := rd.Page(i)
		if p.V.IsNull() {
			continue
		}
		for _, name := range p.Fonts() {
			if _, seen := fonts[name]; !seen {
				fnt := p.Font(name)
				fonts[name] = &fnt
			}
		}
		raw, err := p.GetPlainText(fonts)
		if err != nil {
			continue
		}
		cleaned := cleanText(raw)
		if utf8.RuneCountInString(strings.TrimSpace(cleaned)) > minPDFTextRunes {
			return cleaned, pages, true
		}
	}
	return "", pages, false
}

// extractEmbeddedJPEG scans raw PDF bytes for JPEG SOI..EOI spans — a
// scanned page is an embedded DCTDecode stream stored verbatim, so this
// finds it without touching the PDF object structure at all. Several spans
// can be present (a thumbnail ahead of the real page); the largest decoded
// span wins, which filters small ones without a hand-picked size threshold.
func extractEmbeddedJPEG(data []byte) (image.Image, bool) {
	var best image.Image
	var bestArea int
	for i := 0; i < len(data); {
		idx := bytes.Index(data[i:], []byte{0xFF, 0xD8, 0xFF})
		if idx < 0 {
			break
		}
		start := i + idx
		end := bytes.Index(data[start+2:], []byte{0xFF, 0xD9})
		if end < 0 {
			break
		}
		end = start + 2 + end + 2
		if img, err := jpeg.Decode(bytes.NewReader(data[start:end])); err == nil {
			if area := img.Bounds().Dx() * img.Bounds().Dy(); area > bestArea {
				best, bestArea = img, area
			}
		}
		i = end
	}
	return best, best != nil
}

// --- plain text ----------------------------------------------------------

// fetchText covers md/txt/html/csv/ics/json and anything text/* — mime-
// routed, never by extension, because several rows on the reference device
// have no extension at all and mime lies in both directions (a text/plain
// row can be a binary blob; a directory can pass as a file).
func fetchText(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	if cached, ok := readCache(f, cellW, cellH, proto, docTextVariant); ok {
		return Result{Tier: TierDoc, Meta: splitLines(string(cached))}, nil
	}

	var data []byte
	var err error
	if f.Size > docTextMaxBytes {
		data, err = dev.ExecOut(ctx, ddHead(f.Path, docHeadMiB))
	} else {
		data, err = pullBytes(ctx, dev, f)
	}
	if err != nil {
		return metaResult(f, "pull failed: "+err.Error()), nil
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if !looksPrintable(data) {
		return metaResult(f, "not plain text"), nil
	}
	return docTextResult(f, cellW, cellH, proto, wrapText(cleanText(string(data)), cellW)), nil
}

// looksPrintable sniffs whether data is text worth rendering: valid UTF-8
// with a low control-character ratio. A text/plain-mime row on the
// reference device is actually a serialized binary blob — trust the bytes,
// not the mime.
func looksPrintable(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if !utf8.Valid(data) {
		return false
	}
	control := 0
	for _, b := range data {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			control++
		}
	}
	return float64(control)/float64(len(data)) < 0.01
}

// --- docx ------------------------------------------------------------------

func fetchDocx(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	if cached, ok := readCache(f, cellW, cellH, proto, docTextVariant); ok {
		return Result{Tier: TierDoc, Meta: splitLines(string(cached))}, nil
	}
	data, err := pullBytes(ctx, dev, f)
	if err != nil {
		return metaResult(f, "pull failed: "+err.Error()), nil
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	text, err := extractDocxText(data)
	if err != nil {
		return metaResult(f, "docx — "+err.Error()), nil
	}
	return docTextResult(f, cellW, cellH, proto, wrapText(text, cellW)), nil
}

// extractDocxText strips word/document.xml down to its character data — a
// docx is a zip container, so this is stdlib archive/zip + encoding/xml,
// zero new dependencies.
func extractDocxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	zf := zipFind(zr, "word/document.xml")
	if zf == nil {
		return "", errors.New("no word/document.xml")
	}
	rc, err := zf.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	var b strings.Builder
	dec := xml.NewDecoder(rc)
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.StartElement:
			if t.Name.Local == "p" {
				b.WriteString("\n")
			}
		}
	}
	return b.String(), nil
}

// --- xlsx --------------------------------------------------------------

func fetchXlsx(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	if cached, ok := readCache(f, cellW, cellH, proto, docTextVariant); ok {
		return Result{Tier: TierDoc, Meta: splitLines(string(cached))}, nil
	}
	data, err := pullBytes(ctx, dev, f)
	if err != nil {
		return metaResult(f, "pull failed: "+err.Error()), nil
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	lines, err := extractXlsxSummary(data, cellW)
	if err != nil {
		return metaResult(f, "xlsx — "+err.Error()), nil
	}
	return docTextResult(f, cellW, cellH, proto, lines), nil
}

// extractXlsxSummary reads sheet names from xl/workbook.xml and, when
// present, the head of xl/sharedStrings.xml — spike-verified: sharedStrings
// can be empty on numeric/inline-only sheets, so sheet names alone must be
// enough to produce a result.
func extractXlsxSummary(data []byte, width int) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	wb := zipFind(zr, "xl/workbook.xml")
	if wb == nil {
		return nil, errors.New("no xl/workbook.xml")
	}
	names, err := sheetNames(wb)
	if err != nil || len(names) == 0 {
		return nil, errors.New("no sheet names found")
	}
	lines := wrapText("sheets: "+strings.Join(names, ", "), width)

	if ss := zipFind(zr, "xl/sharedStrings.xml"); ss != nil {
		if strs, err := sharedStringsHead(ss, 40); err == nil && len(strs) > 0 {
			lines = append(lines, "")
			lines = append(lines, wrapText(strings.Join(strs, "  "), width)...)
		}
	}
	return lines, nil
}

func sheetNames(zf *zip.File) ([]string, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var wb struct {
		Sheets struct {
			Sheet []struct {
				Name string `xml:"name,attr"`
			} `xml:"sheet"`
		} `xml:"sheets"`
	}
	if err := xml.NewDecoder(rc).Decode(&wb); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(wb.Sheets.Sheet))
	for _, s := range wb.Sheets.Sheet {
		names = append(names, s.Name)
	}
	return names, nil
}

func sharedStringsHead(zf *zip.File, n int) ([]string, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var sst struct {
		SI []struct {
			T string `xml:"t"`
		} `xml:"si"`
	}
	if err := xml.NewDecoder(rc).Decode(&sst); err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for _, si := range sst.SI {
		if si.T == "" {
			continue
		}
		out = append(out, si.T)
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

func zipFind(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// --- zip family (stretch) -----------------------------------------------

// fetchZipListing covers zip/apk/epub/pkpass/cbz — all zip containers, all
// shown as one entry listing rather than trying to interpret contents.
func fetchZipListing(ctx context.Context, dev device.Device, f device.File, cellW, cellH int, proto Protocol) (Result, error) {
	if cached, ok := readCache(f, cellW, cellH, proto, docTextVariant); ok {
		return Result{Tier: TierDoc, Meta: splitLines(string(cached))}, nil
	}
	data, err := pullBytes(ctx, dev, f)
	if err != nil {
		return metaResult(f, "pull failed: "+err.Error()), nil
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return metaResult(f, "not a valid zip container"), nil
	}
	var lines []string
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		lines = append(lines, fmt.Sprintf("%8s  %s", humanBytes(int64(zf.UncompressedSize64)), zf.Name))
		if len(lines) >= maxDocLines {
			break
		}
	}
	if len(lines) == 0 {
		return metaResult(f, "empty archive"), nil
	}
	return docTextResult(f, cellW, cellH, proto, lines), nil
}
