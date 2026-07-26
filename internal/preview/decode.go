package preview

import (
	"bytes"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
)

var errUnsupportedFormat = errors.New("preview: unsupported image format")

// maxPreviewPixels bounds the decoded/rendered image so a 48 MP photo isn't
// downscaled from full resolution on every cursor rest; it only needs to
// look right at a few hundred pixels.
const maxPreviewDim = 800

// decodeStdlib handles the 99.4% case: JPEG, PNG, GIF via Go's stdlib.
func decodeStdlib(mime string, data []byte) (image.Image, error) {
	r := bytes.NewReader(data)
	switch {
	case mimeIs(mime, "image/jpeg"):
		return jpeg.Decode(r)
	case mimeIs(mime, "image/png"):
		return png.Decode(r)
	case mimeIs(mime, "image/gif"):
		return gif.Decode(r) // first frame only — this is a still preview
	}
	// Fall back to format sniffing: extension-derived mime types are not
	// always trustworthy, but the magic bytes always are.
	if img, _, err := image.Decode(bytes.NewReader(data)); err == nil {
		return img, nil
	}
	return nil, errUnsupportedFormat
}

func mimeIs(mime, want string) bool {
	return len(mime) >= len(want) && mime[:len(want)] == want
}

// downscale fits img within w×h terminal cells using a CatmullRom filter —
// there's no fast path here worth taking since previews are cached after the
// first decode.
func downscale(img image.Image, cellW, cellH int) image.Image {
	pxW, pxH := targetPixels(img.Bounds().Dx(), img.Bounds().Dy(), cellW, cellH)
	dst := image.NewRGBA(image.Rect(0, 0, pxW, pxH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	return dst
}

// targetPixels fits the source aspect ratio inside cellW×cellH terminal
// cells, assuming the classic ~2:1 character cell (each row of characters
// covers roughly two rows of pixels, per half-block rendering).
func targetPixels(srcW, srcH, cellW, cellH int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		srcW, srcH = 1, 1
	}
	maxW := min(cellW, maxPreviewDim)
	maxH := min(cellH*2, maxPreviewDim)
	scale := min(float64(maxW)/float64(srcW), float64(maxH)/float64(srcH))
	if scale > 1 {
		scale = 1
	}
	w := max(1, int(float64(srcW)*scale))
	h := max(1, int(float64(srcH)*scale))
	// Half-block rendering needs an even pixel height so each character row
	// maps to exactly two pixel rows.
	if h%2 != 0 {
		h++
	}
	return w, h
}
