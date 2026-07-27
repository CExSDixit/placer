// placer — fuzzy-searchable ADB file browser with multi-select and batch pull.
//
// Browse, search, select, transfer, and preview images, video frames and audio
// waveforms inline. Document previews land in phase 4.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/preview"
	"github.com/CExSDixit/placer/internal/ui"
)

var version = "0.4.4-phase3"

func main() {
	var (
		serial   = flag.String("s", "", "device serial (default: the only attached device)")
		adbBin   = flag.String("adb", "adb", "path to the adb binary")
		fake     = flag.Bool("fake", false, "run against a synthetic library (no device needed)")
		fixtures = flag.String("fixtures", "", "run against recorded content-query fixtures in this directory")
		showVer  = flag.Bool("version", false, "print version and exit")
		render   = flag.String("render", "", "force the image renderer for this run: kitty|iterm|sixel|quadrant|halfblock")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("placer", version)
		return
	}

	dev, err := open(*fake, *fixtures, *adbBin, *serial)
	if err != nil {
		fmt.Fprintln(os.Stderr, "placer:", err)
		os.Exit(1)
	}

	// The media cache holds whole audio files — hundreds of MB each on the
	// reference device — so trim it to budget before adding more.
	preview.PruneCaches()

	// Both must run before the Bubble Tea program starts reading stdin: the
	// sixel probe puts the terminal in raw mode and reads its response itself.
	preview.DetectCellPixels()
	proto := preview.DetectProtocol()

	// -render forces the renderer for this run only. Unlike `:set render` it is
	// never persisted, so it cannot follow you into a terminal where it
	// silently produces a blank pane — which is exactly what a saved "kitty"
	// did inside herdr, where graphics escapes are dropped.
	if *render != "" {
		forced, ok := preview.ParseProtocol(*render)
		if !ok {
			fmt.Fprintln(os.Stderr, "placer: -render must be one of kitty|iterm|sixel|quadrant|halfblock")
			os.Exit(1)
		}
		proto = forced
	}

	p := tea.NewProgram(ui.New(dev, proto), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "placer:", err)
		os.Exit(1)
	}
}

func open(fake bool, fixtures, adbBin, serial string) (device.Device, error) {
	if fake {
		return device.Synthetic(1), nil
	}
	if fixtures != "" {
		return device.LoadFixtures(fixtures)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return device.NewADB(ctx, adbBin, serial)
}
