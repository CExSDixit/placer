package preview

import (
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Active capability probing, because environment sniffing cannot see through a
// terminal multiplexer.
//
// rasterm decides "is this kitty-capable" from KITTY_WINDOW_ID and
// TERM_PROGRAM. Inside herdr those describe the multiplexer, not the terminal
// underneath it: a herdr pane hosted by Ghostty reports
// TERM_PROGRAM=Apple_Terminal, TERM=xterm-256color and no KITTY_WINDOW_ID. So
// placer fell back to block rendering even though herdr passes kitty graphics
// straight through to Ghostty and the images render fine.
//
// The fix is to ask the terminal instead of guessing: the kitty protocol has a
// query action (`a=q`) whose whole purpose is this, and a multiplexer that
// forwards graphics forwards the query and its reply too. Same technique used
// to diagnose the stdin-acknowledgement trap — write a candidate command to
// /dev/tty in raw mode and see what comes back.
//
// Must run before Bubble Tea starts reading stdin, alongside DetectProtocol.

// probeTimeout is how long to wait for a reply. Terminals answer immediately;
// this only has to cover the round trip through any multiplexers in between.
const probeTimeout = 250 * time.Millisecond

// kittyProbeID is the image id used for the query. Nothing is ever displayed:
// `a=q` asks the terminal to validate the transmission and reply, then discard
// it.
const kittyProbeID = 7302

// ProbeKitty reports whether the terminal answers a kitty graphics query.
//
// Returns false on any error, on a terminal that is not a tty, or on timeout —
// a probe that cannot be run is not evidence of support.
func ProbeKitty() bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer tty.Close()
	if !term.IsTerminal(int(tty.Fd())) {
		return false
	}

	st, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		return false
	}
	defer term.Restore(int(tty.Fd()), st)

	// Read in the background: a terminal that ignores the query sends nothing
	// at all, so a blocking read would hang forever.
	var mu sync.Mutex
	var got strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				mu.Lock()
				got.Write(buf[:n])
				full := strings.Contains(got.String(), "\x1b\\")
				mu.Unlock()
				if full {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// A 1×1 RGB image, transmitted as a query. `q=0` because we WANT the
	// reply here — this is the one place a response is the point.
	if _, err := tty.WriteString(
		"\x1b_Ga=q,i=" + itoa(kittyProbeID) + ",s=1,v=1,f=24,t=d;AAAA\x1b\\",
	); err != nil {
		return false
	}

	select {
	case <-done:
	case <-time.After(probeTimeout):
	}

	mu.Lock()
	reply := got.String()
	mu.Unlock()

	// Any well-formed answer naming our id means the terminal understood the
	// graphics protocol. "OK" is success; an error code (ENOTSUPPORTED etc.)
	// still proves it parsed the command, but only OK means it will display.
	return strings.Contains(reply, "i="+itoa(kittyProbeID)) && strings.Contains(reply, "OK")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
