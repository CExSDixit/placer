// capture-fixtures records raw `content query` output from an attached device
// into a fixtures directory, so the parser and TUI can be exercised against
// real rows with no phone present.
//
//	go run ./cmd/capture-fixtures -out testdata/fixtures
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

func main() {
	out := flag.String("out", "testdata/fixtures", "directory to write fixtures into")
	serial := flag.String("s", "", "device serial")
	adbBin := flag.String("adb", "adb", "path to adb")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dev, err := device.NewADB(ctx, *adbBin, *serial)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capture-fixtures:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for name, coll := range map[string]device.Collection{
		"images": device.Images, "video": device.Video, "audio": device.Audio,
		"downloads": device.Downloads,
	} {
		q := device.Query{Coll: coll, Projection: device.StandardProjection(coll)}
		raw, err := dev.RawQuery(ctx, q)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %-10s FAILED: %v\n", name, err)
			continue
		}
		path := filepath.Join(*out, name+".txt")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}
		rows := device.ParseRows(string(raw), q.Projection)
		fmt.Printf("  %-10s %6d rows  %8d bytes  -> %s\n", name, len(rows), len(raw), path)
	}
	fmt.Println("\nRun the TUI against these with:  placer -fixtures", *out)
}
