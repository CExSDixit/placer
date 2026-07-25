package device

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ADB drives the system `adb` binary. This is the only part of adbfz that
// needs real hardware to verify, so it is kept as thin as possible.
type ADB struct {
	Bin    string // defaults to "adb"
	serial string
}

// Attached lists connected devices, in `adb devices` order.
func Attached(ctx context.Context, bin string) ([]string, error) {
	if bin == "" {
		bin = "adb"
	}
	out, err := exec.CommandContext(ctx, bin, "devices").Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}
	var serials []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// Skip the "List of devices attached" header and offline/unauthorized
		// entries — talking to those fails in confusing ways later.
		if len(f) >= 2 && f[1] == "device" {
			serials = append(serials, f[0])
		}
	}
	return serials, nil
}

// NewADB binds to a specific serial, or to the only attached device when
// serial is empty.
func NewADB(ctx context.Context, bin, serial string) (*ADB, error) {
	if bin == "" {
		bin = "adb"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("%s not found on PATH: %w", bin, err)
	}
	if serial == "" {
		serials, err := Attached(ctx, bin)
		if err != nil {
			return nil, err
		}
		switch len(serials) {
		case 0:
			return nil, fmt.Errorf("no authorized device attached (check USB debugging and the on-device RSA prompt)")
		case 1:
			serial = serials[0]
		default:
			return nil, fmt.Errorf("%d devices attached; pass -s <serial>: %s", len(serials), strings.Join(serials, " "))
		}
	}
	return &ADB{Bin: bin, serial: serial}, nil
}

func (a *ADB) Serial() string { return a.serial }

func (a *ADB) args(rest ...string) []string {
	return append([]string{"-s", a.serial}, rest...)
}

// shellQuote wraps a value for the *device's* shell in single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildQuery renders a `content query` invocation as ONE string.
//
// This must stay a single argument: `adb shell` re-joins argv and the device's
// shell re-splits it on whitespace, so passing --sort "datetaken DESC" as a
// separate arg arrives on the device as two tokens and fails. Same bug already
// hit and fixed in cookbooks' pull-recent-photos.sh.
func buildQuery(q Query) string {
	var b strings.Builder
	b.WriteString("content query --uri ")
	b.WriteString(string(q.Coll))
	if len(q.Projection) > 0 {
		b.WriteString(" --projection ")
		b.WriteString(strings.Join(q.Projection, ":"))
	}
	if q.Where != "" {
		b.WriteString(" --where ")
		b.WriteString(shellQuote(q.Where))
	}
	if q.Sort != "" {
		b.WriteString(" --sort ")
		b.WriteString(shellQuote(q.Sort))
	}
	return b.String()
}

// RawQuery returns unparsed `content query` output, which is what the fixture
// capture tool records so the parser can be tested against real device rows.
func (a *ADB) RawQuery(ctx context.Context, q Query) ([]byte, error) {
	cmd := exec.CommandContext(ctx, a.Bin, a.args("shell", buildQuery(q))...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("content query %s: %w: %s", q.Coll, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (a *ADB) Query(ctx context.Context, q Query) ([]map[string]string, error) {
	out, err := a.RawQuery(ctx, q)
	if err != nil {
		return nil, err
	}
	return ParseRows(string(out), q.Projection), nil
}

// ExecOut uses `adb exec-out`, which is binary-clean. `adb shell` would
// translate \n to \r\n and corrupt any image or media bytes.
func (a *ADB) ExecOut(ctx context.Context, cmd string) ([]byte, error) {
	c := exec.CommandContext(ctx, a.Bin, a.args("exec-out", cmd)...)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("exec-out %q: %w: %s", cmd, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// adb reports transfer progress as "[ 45%] /path/to/file", rewriting the line
// with \r rather than emitting newlines.
var pctRe = regexp.MustCompile(`\[\s*(\d{1,3})%\]`)

func (a *ADB) Pull(ctx context.Context, remote, local string, prog func(Progress)) error {
	if err := os.MkdirAll(dirOf(local), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, a.Bin, a.args("pull", remote, local)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	if prog != nil {
		go func() {
			buf := make([]byte, 4096)
			last := -1
			for {
				n, err := stdout.Read(buf)
				if n > 0 {
					if m := pctRe.FindAllSubmatch(buf[:n], -1); len(m) > 0 {
						p, _ := strconv.Atoi(string(m[len(m)-1][1]))
						if p != last {
							last = p
							prog(Progress{Path: remote, Percent: p})
						}
					}
				}
				if err != nil {
					return
				}
			}
		}()
	} else {
		go io.Copy(io.Discard, stdout)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("pull %s: %w: %s", remote, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}
