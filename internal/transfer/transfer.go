// Package transfer batch-pulls the selection to a local destination.
package transfer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/CExSDixit/placer/internal/device"
)

// Policy decides what happens when the destination file already exists.
type Policy int

const (
	Skip Policy = iota
	Overwrite
	Rename
)

var PolicyNames = map[Policy]string{Skip: "skip", Overwrite: "overwrite", Rename: "rename"}

// Workers is deliberately small: measured throughput is 23-41 MB/s, close to
// USB saturation, so extra workers buy nothing and only muddy the progress
// display.
const Workers = 2

// Event reports progress for one file in the batch.
type Event struct {
	Index   int
	File    device.File
	Local   string
	Percent int
	Done    bool
	Skipped bool
	Err     error
}

// Result summarises a finished batch.
type Result struct {
	Pulled  int
	Skipped int
	Failed  []Event
	Bytes   int64
	Elapsed time.Duration
}

// Run pulls every file to dest, emitting events until it closes the channel.
func Run(ctx context.Context, dev device.Device, files []device.File, dest string, pol Policy, events chan<- Event) Result {
	start := time.Now()
	defer close(events)

	var (
		mu  sync.Mutex
		res Result
	)
	jobs := make(chan int)

	var wg sync.WaitGroup
	for w := 0; w < Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				f := files[i]
				ev := pull(ctx, dev, f, dest, pol, i, events)
				mu.Lock()
				switch {
				case ev.Err != nil:
					res.Failed = append(res.Failed, ev)
				case ev.Skipped:
					res.Skipped++
				default:
					res.Pulled++
					res.Bytes += f.Size
				}
				mu.Unlock()
				events <- ev
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i := range files {
			select {
			case <-ctx.Done():
				return
			case jobs <- i:
			}
		}
	}()

	wg.Wait()
	res.Elapsed = time.Since(start)
	return res
}

func pull(ctx context.Context, dev device.Device, f device.File, dest string, pol Policy, idx int, events chan<- Event) Event {
	ev := Event{Index: idx, File: f, Done: true}

	name := f.Name
	if name == "" {
		name = filepath.Base(f.Path)
	}
	local := filepath.Join(dest, name)

	if _, err := os.Stat(local); err == nil {
		switch pol {
		case Skip:
			ev.Skipped = true
			ev.Local = local
			return ev
		case Rename:
			local = uniquePath(local)
		}
	}
	ev.Local = local

	if f.Path == "" {
		ev.Err = fmt.Errorf("no device path (scoped storage); content:// streaming not implemented in phase 1")
		return ev
	}

	err := dev.Pull(ctx, f.Path, local, func(p device.Progress) {
		select {
		case events <- Event{Index: idx, File: f, Local: local, Percent: p.Percent}:
		default: // never block a transfer on a slow consumer
		}
	})
	if err != nil {
		ev.Err = err
		return ev
	}

	// Verify size when MediaStore claimed one.
	if st, serr := os.Stat(local); serr == nil && f.Size > 0 && st.Size() != f.Size {
		ev.Err = fmt.Errorf("size mismatch: expected %d, got %d", f.Size, st.Size())
		return ev
	}

	// Set local mtime from capture time so pulled photos sort correctly in
	// Finder and in downstream tooling.
	if t := f.SortTime(); !t.IsZero() {
		_ = os.Chtimes(local, t, t)
	}
	ev.Percent = 100
	return ev
}

// uniquePath turns /d/a.jpg into /d/a (1).jpg, /d/a (2).jpg, ...
func uniquePath(p string) string {
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return p
}
