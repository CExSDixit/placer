package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sahilm/fuzzy"
)

// destModel is a local-filesystem directory browser with the same vim keys as
// the main list, used to choose where a batch lands.
type destModel struct {
	cwd       string
	dirs      []string // directory names in cwd
	shown     []string // after filter
	cursor    int
	offset    int
	filter    string
	filtering bool
	mkdir     bool
	input     textinput.Model
	bookmarks []string
	recents   []string
	err       string
	h         int
}

func newDest(start string, bookmarks, recents []string) *destModel {
	ti := textinput.New()
	ti.Prompt = ""
	d := &destModel{cwd: start, bookmarks: bookmarks, recents: recents, input: ti, h: 10}
	if st, err := os.Stat(start); err != nil || !st.IsDir() {
		home, _ := os.UserHomeDir()
		d.cwd = home
	}
	d.read()
	return d
}

func (d *destModel) read() {
	d.dirs = nil
	d.err = ""
	ents, err := os.ReadDir(d.cwd)
	if err != nil {
		d.err = err.Error()
	}
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			d.dirs = append(d.dirs, e.Name())
		}
	}
	sort.Slice(d.dirs, func(i, j int) bool {
		return strings.ToLower(d.dirs[i]) < strings.ToLower(d.dirs[j])
	})
	d.applyFilter()
}

func (d *destModel) applyFilter() {
	if d.filter == "" {
		d.shown = d.dirs
	} else {
		res := fuzzy.Find(d.filter, d.dirs)
		d.shown = make([]string, 0, len(res))
		for _, m := range res {
			d.shown = append(d.shown, d.dirs[m.Index])
		}
	}
	if d.cursor >= len(d.shown) {
		d.cursor = max(0, len(d.shown)-1)
	}
	d.clampOffset()
}

func (d *destModel) clampOffset() {
	if d.cursor < d.offset {
		d.offset = d.cursor
	}
	if d.h > 0 && d.cursor >= d.offset+d.h {
		d.offset = d.cursor - d.h + 1
	}
	if d.offset < 0 {
		d.offset = 0
	}
}

func (d *destModel) enter() {
	if d.cursor < len(d.shown) {
		d.cwd = filepath.Join(d.cwd, d.shown[d.cursor])
		d.cursor, d.offset, d.filter = 0, 0, ""
		d.read()
	}
}

func (d *destModel) up() {
	parent := filepath.Dir(d.cwd)
	if parent != d.cwd {
		prev := filepath.Base(d.cwd)
		d.cwd = parent
		d.cursor, d.offset, d.filter = 0, 0, ""
		d.read()
		for i, n := range d.shown { // land on the directory we came from
			if n == prev {
				d.cursor = i
				d.clampOffset()
				break
			}
		}
	}
}

// destChosenMsg is emitted when the user confirms a destination.
type destChosenMsg struct{ path string }
type destCancelledMsg struct{}

func (d *destModel) Update(msg tea.Msg) (*destModel, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	k := km.String()

	if d.mkdir {
		switch k {
		case "esc", "ctrl+c":
			d.mkdir = false
			d.input.Blur()
			d.input.SetValue("")
		case "enter":
			name := strings.TrimSpace(d.input.Value())
			d.mkdir = false
			d.input.Blur()
			d.input.SetValue("")
			if name != "" {
				if err := os.MkdirAll(filepath.Join(d.cwd, name), 0o755); err != nil {
					d.err = err.Error()
				} else {
					d.read()
					for i, n := range d.shown {
						if n == name {
							d.cursor = i
							d.clampOffset()
						}
					}
				}
			}
		default:
			var cmd tea.Cmd
			d.input, cmd = d.input.Update(msg)
			return d, cmd
		}
		return d, nil
	}

	if d.filtering {
		switch k {
		case "esc":
			d.filtering = false
			d.input.Blur()
		case "enter":
			d.filtering = false
			d.input.Blur()
		case "ctrl+c":
			d.filtering = false
			d.filter = ""
			d.input.Blur()
			d.input.SetValue("")
			d.applyFilter()
		default:
			var cmd tea.Cmd
			d.input, cmd = d.input.Update(msg)
			d.filter = d.input.Value()
			d.applyFilter()
			return d, cmd
		}
		return d, nil
	}

	switch k {
	case "j", "down":
		if d.cursor < len(d.shown)-1 {
			d.cursor++
			d.clampOffset()
		}
	case "k", "up":
		if d.cursor > 0 {
			d.cursor--
			d.clampOffset()
		}
	case "g":
		d.cursor, d.offset = 0, 0
	case "G":
		d.cursor = max(0, len(d.shown)-1)
		d.clampOffset()
	case "l", "right":
		d.enter()
	case "h", "left":
		d.up()
	case "/":
		d.filtering = true
		d.input.SetValue("")
		d.input.Focus()
	case "m":
		d.mkdir = true
		d.input.SetValue("")
		d.input.Focus()
	case "b":
		if len(d.bookmarks) > 0 {
			d.cwd = d.bookmarks[0]
			d.bookmarks = append(d.bookmarks[1:], d.bookmarks[0]) // cycle
			d.cursor, d.offset, d.filter = 0, 0, ""
			d.read()
		}
	case "enter":
		return d, func() tea.Msg { return destChosenMsg{d.cwd} }
	case "esc", "q", "ctrl+c":
		return d, func() tea.Msg { return destCancelledMsg{} }
	}
	return d, nil
}

func (d *destModel) View(w int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(" destination ") + " " + pathStyle.Render(truncLeft(d.cwd, w-16)) + "\n")

	if d.err != "" {
		b.WriteString(errStyle.Render("  "+d.err) + "\n")
	}
	if len(d.shown) == 0 {
		b.WriteString(dimStyle.Render("  (no subdirectories)") + "\n")
	}
	end := min(len(d.shown), d.offset+d.h)
	for i := d.offset; i < end; i++ {
		line := "  " + d.shown[i] + "/"
		if i == d.cursor {
			b.WriteString(cursorStyle.Render("▸ "+d.shown[i]+"/") + "\n")
		} else {
			b.WriteString(line + "\n")
		}
	}

	switch {
	case d.mkdir:
		b.WriteString("\n" + promptStyle.Render("mkdir: ") + d.input.View())
	case d.filtering:
		b.WriteString("\n" + promptStyle.Render("/") + d.input.View())
	default:
		hint := "j/k move · l enter · h up · / filter · m mkdir · b bookmark · enter choose · esc cancel"
		if len(d.recents) > 0 {
			hint = "recent: " + truncLeft(d.recents[0], 40) + "\n" + hint
		}
		b.WriteString("\n" + dimStyle.Render(hint))
	}
	return b.String()
}

func (d *destModel) Summary() string {
	return fmt.Sprintf("%s (%d dirs)", d.cwd, len(d.shown))
}
