// placer — fuzzy-searchable ADB file browser with multi-select and batch pull.
//
// Phase 1: browse, search, select, transfer. Previews land in phase 2.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mishrasidhant/placer/internal/device"
	"github.com/mishrasidhant/placer/internal/ui"
)

var version = "0.1.0-phase1"

func main() {
	var (
		serial   = flag.String("s", "", "device serial (default: the only attached device)")
		adbBin   = flag.String("adb", "adb", "path to the adb binary")
		fake     = flag.Bool("fake", false, "run against a synthetic library (no device needed)")
		fixtures = flag.String("fixtures", "", "run against recorded content-query fixtures in this directory")
		showVer  = flag.Bool("version", false, "print version and exit")
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

	p := tea.NewProgram(ui.New(dev), tea.WithAltScreen())
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
