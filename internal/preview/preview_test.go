package preview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

func sampleDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "sample")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no testdata/sample; see rabbitholes/placer-phase2-handoff.md")
	}
	return dir
}

func sampleFile(t *testing.T, dir, name, mime string) device.File {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Skipf("sample %s not present: %v", name, err)
	}
	return device.File{
		Path: name, // Fake.Pull keys off filepath.Base, so this alone is enough
		Name: name,
		Size: fi.Size(),
		Mime: mime,
		Coll: device.Images,
	}
}

// fakeDevWithCache points os.UserCacheDir() at a fresh temp directory (by
// overriding HOME/XDG_CACHE_HOME, whichever the platform's UserCacheDir
// consults) so tests never read or pollute the real ~/.cache/placer/thumbs.
// t.Setenv restores both automatically at test end.
func fakeDevWithCache(t *testing.T, srcDir string) device.Device {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, ".cache"))
	return &device.Fake{SrcDir: srcDir, Latency: time.Millisecond, Speed: 100 << 20}
}

func TestFetch_JPEG(t *testing.T) {
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "PXL_20260404_154433428.PORTRAIT.ORIGINAL.jpg", "image/jpeg")

	res, err := Fetch(context.Background(), dev, f, 40, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierImage {
		t.Fatalf("tier = %v, want TierImage", res.Tier)
	}
	if len(res.Rendered) == 0 {
		t.Fatal("expected non-empty rendered output")
	}

	// Second fetch must hit the cache and return byte-identical output.
	res2, err := Fetch(context.Background(), dev, f, 40, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if string(res2.Rendered) != string(res.Rendered) {
		t.Fatal("cached render differs from first render")
	}
}

func TestFetch_PNG(t *testing.T) {
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "Screenshot_20240314-211047.png", "image/png")

	res, err := Fetch(context.Background(), dev, f, 40, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierImage || len(res.Rendered) == 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestFetch_GIF(t *testing.T) {
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "unnamed.gif", "image/gif")

	res, err := Fetch(context.Background(), dev, f, 40, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierImage || len(res.Rendered) == 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestFetch_DNG(t *testing.T) {
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "PXL_20260721_162134343.RAW-02.ORIGINAL.dng", "image/x-adobe-dng")

	res, err := Fetch(context.Background(), dev, f, 40, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierDNG || len(res.Rendered) == 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestFetch_HEIC_MetadataOnly(t *testing.T) {
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "IMG_20250322_154811.heic", "image/heic")

	res, err := Fetch(context.Background(), dev, f, 40, 20, ProtoHalfBlock)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Tier != TierMeta || len(res.Rendered) != 0 {
		t.Fatalf("heic must be metadata-only, got %+v", res)
	}
}

func TestFetch_CancelledContext(t *testing.T) {
	dir := sampleDir(t)
	dev := fakeDevWithCache(t, dir)
	f := sampleFile(t, dir, "PXL_20260404_154433428.PORTRAIT.ORIGINAL.jpg", "image/jpeg")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Fetch(ctx, dev, f, 40, 20, ProtoHalfBlock); err == nil {
		t.Fatal("expected an error from a pre-cancelled context")
	}
}
