package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/index"
	"github.com/CExSDixit/placer/internal/session"
	"github.com/CExSDixit/placer/internal/transfer"
)

type mode int

const (
	modeNormal mode = iota
	modeSearch
	modeCommand
	modeDest
	modeSelection
	modeTransfer
	modeHelp
)

// Model is the root Bubble Tea model.
type Model struct {
	dev  device.Device
	ix   *index.Index
	cfg  session.Config
	man  *session.Manifest
	pol  transfer.Policy
	dest *destModel

	tab    index.Tab
	sortBy index.SortMode
	query  string
	view   index.View

	cursor, offset int
	mode           mode
	prev           mode
	input          textinput.Model
	pending        string // multi-key vim prefix, e.g. "g"
	visual         bool
	anchor         int

	// selection review pane
	selCursor, selOffset int

	// transfer state
	tprog     map[int]int
	tfiles    []device.File
	tdone     int
	tfailed   []transfer.Event
	tevents   chan transfer.Event
	tcomplete bool
	tcancel   context.CancelFunc
	tstarted  time.Time
	telapsed  time.Duration

	w, h    int
	status  string
	errMsg  string
	loading bool
	quit    bool
}

func New(dev device.Device) Model {
	ti := textinput.New()
	ti.Prompt = ""
	cfg := session.LoadConfig()
	return Model{
		dev:     dev,
		cfg:     cfg,
		man:     session.LoadManifest(session.ManifestPath()),
		input:   ti,
		loading: true,
		tprog:   map[int]int{},
		w:       80,
		h:       24,
	}
}

type indexLoadedMsg struct {
	ix   *index.Index
	errs []error
}
type statusExpiredMsg struct{}
type transferEventMsg transfer.Event
type transferDoneMsg struct{}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadIndex, textinput.Blink)
}

func (m Model) loadIndex() tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ix, errs := index.Load(ctx, m.dev)
	return indexLoadedMsg{ix: ix, errs: errs}
}

func (m *Model) setStatus(s string) tea.Cmd {
	m.status = s
	return tea.Tick(4*time.Second, func(time.Time) tea.Msg { return statusExpiredMsg{} })
}

func (m *Model) rebuildView() {
	if m.ix == nil {
		return
	}
	m.view = m.ix.Build(m.tab, m.query, m.sortBy)
	if m.cursor >= len(m.view.Files) {
		m.cursor = max(0, len(m.view.Files)-1)
	}
	m.clampOffset()
}

func (m *Model) listHeight() int {
	// header(2) + column head(1) + footer(2)
	return max(1, m.h-6)
}

func (m *Model) clampOffset() {
	h := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *Model) cur() (device.File, bool) {
	if m.cursor >= 0 && m.cursor < len(m.view.Files) {
		return m.view.Files[m.cursor], true
	}
	return device.File{}, false
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if m.dest != nil {
			m.dest.h = max(3, m.h-10)
		}
		m.clampOffset()
		return m, nil

	case indexLoadedMsg:
		m.loading = false
		m.ix = msg.ix
		if len(msg.errs) > 0 {
			m.errMsg = fmt.Sprintf("%d collection(s) failed: %v", len(msg.errs), msg.errs[0])
		}
		m.rebuildView()
		c := m.ix.Counts()
		return m, m.setStatus(fmt.Sprintf("indexed %d files (%d photos, %d video, %d audio)",
			len(m.ix.All), c[index.TabPhotos], c[index.TabVideo], c[index.TabAudio]))

	case statusExpiredMsg:
		m.status = ""
		return m, nil

	case destChosenMsg:
		m.cfg.NoteRecent(msg.path)
		_ = m.cfg.Save()
		m.mode = modeNormal
		return m, m.setStatus("destination: " + msg.path)

	case destCancelledMsg:
		m.mode = modeNormal
		return m, nil

	case transferEventMsg:
		ev := transfer.Event(msg)
		if ev.Done {
			m.tdone++
			if ev.Err != nil {
				m.tfailed = append(m.tfailed, ev)
			}
			m.tprog[ev.Index] = 100
		} else {
			m.tprog[ev.Index] = ev.Percent
		}
		return m, waitEvent(m.tevents)

	case transferDoneMsg:
		m.tcomplete = true
		m.telapsed = time.Since(m.tstarted)
		m.tcancel = nil
		return m, nil
	}

	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	return m.handleKey(km)
}

func waitEvent(ch chan transfer.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return transferDoneMsg{}
		}
		return transferEventMsg(ev)
	}
}

func (m Model) handleKey(km tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := km.String()

	switch m.mode {
	case modeDest:
		d, cmd := m.dest.Update(km)
		m.dest = d
		return m, cmd

	case modeSearch:
		switch k {
		case "esc", "enter":
			m.mode = modeNormal
			m.input.Blur()
			return m, nil
		case "ctrl+c":
			m.mode = modeNormal
			m.query = ""
			m.input.SetValue("")
			m.input.Blur()
			m.rebuildView()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(km)
		m.query = m.input.Value()
		m.cursor, m.offset = 0, 0
		m.rebuildView()
		return m, cmd

	case modeCommand:
		switch k {
		case "esc", "ctrl+c":
			m.mode = modeNormal
			m.input.Blur()
			return m, nil
		case "enter":
			cmdline := m.input.Value()
			m.mode = modeNormal
			m.input.Blur()
			m.input.SetValue("")
			return m.runCommand(cmdline)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(km)
		return m, cmd

	case modeHelp:
		m.mode = m.prev
		return m, nil

	case modeSelection:
		return m.handleSelectionKey(k)

	case modeTransfer:
		switch k {
		case "esc", "q", "enter":
			if m.tcomplete {
				// Successfully transferred files leave the manifest; failures
				// stay, so retrying is just another p. Build the keep set
				// BEFORE clearing transfer state.
				keep := map[string]bool{}
				for _, ev := range m.tfailed {
					keep[ev.File.Path] = true
				}
				for _, f := range m.tfiles {
					if !keep[f.Path] {
						m.man.Remove(f)
					}
				}
				_ = m.man.Save(session.ManifestPath())

				m.mode = modeNormal
				m.tprog = map[int]int{}
				m.tdone = 0
				m.tfailed = nil
				m.tfiles = nil
				m.tcomplete = false
			}
		case "ctrl+c":
			if m.tcancel != nil {
				m.tcancel()
				return m, m.setStatus("cancelling transfer…")
			}
		}
		return m, nil
	}

	return m.handleNormalKey(k)
}

func (m Model) handleNormalKey(k string) (tea.Model, tea.Cmd) {
	// Multi-key sequences: only "g" is pending-capable in phase 1.
	if m.pending == "g" {
		m.pending = ""
		switch k {
		case "g":
			m.cursor, m.offset = 0, 0
			return m, nil
		case "t":
			m.tab = (m.tab + 1) % index.Tab(len(index.TabNames))
			m.cursor, m.offset = 0, 0
			m.rebuildView()
			return m, nil
		case "T":
			m.tab = (m.tab - 1 + index.Tab(len(index.TabNames))) % index.Tab(len(index.TabNames))
			m.cursor, m.offset = 0, 0
			m.rebuildView()
			return m, nil
		}
		return m, nil
	}

	n := len(m.view.Files)

	switch k {
	case "q":
		if m.man.Len() > 0 {
			return m, m.setStatus(fmt.Sprintf("%d file(s) selected — :q! to discard, p to pull, s to review", m.man.Len()))
		}
		m.quit = true
		return m, tea.Quit
	case "ctrl+c":
		_ = m.man.Save(session.ManifestPath())
		m.quit = true
		return m, tea.Quit

	case "j", "down":
		if m.cursor < n-1 {
			m.cursor++
			m.clampOffset()
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
			m.clampOffset()
		}
	case "ctrl+d":
		m.cursor = min(n-1, m.cursor+m.listHeight()/2)
		m.clampOffset()
	case "ctrl+u":
		m.cursor = max(0, m.cursor-m.listHeight()/2)
		m.clampOffset()
	case "g":
		m.pending = "g"
	case "G":
		m.cursor = max(0, n-1)
		m.clampOffset()

	case "tab", " ", "enter":
		if m.visual {
			m.toggleRange()
			m.visual = false
		} else if f, ok := m.cur(); ok {
			m.man.Toggle(f)
			if m.cursor < n-1 {
				m.cursor++
				m.clampOffset()
			}
		}
	case "y":
		if m.visual {
			m.addRange()
			m.visual = false
			return m, m.setStatus(fmt.Sprintf("%d selected", m.man.Len()))
		}
		if f, ok := m.cur(); ok {
			m.man.Add(f)
			return m, m.setStatus(fmt.Sprintf("%d selected", m.man.Len()))
		}
	case "v":
		m.visual = !m.visual
		m.anchor = m.cursor
	case "V":
		for _, f := range m.view.Files {
			m.man.Add(f)
		}
		return m, m.setStatus(fmt.Sprintf("selected all %d visible (%d total)", len(m.view.Files), m.man.Len()))
	case "esc":
		if m.visual {
			m.visual = false
		} else if m.query != "" {
			m.query = ""
			m.input.SetValue("")
			m.rebuildView()
		}

	case "s":
		m.mode = modeSelection
		m.selCursor, m.selOffset = 0, 0
	case "d":
		m.dest = newDest(m.cfg.Dest, m.cfg.Bookmarks, m.cfg.Recents)
		m.dest.h = max(3, m.h-10)
		m.mode = modeDest
	case "p":
		return m.startTransfer()

	case "/":
		m.mode = modeSearch
		m.input.SetValue(m.query)
		m.input.Focus()
		return m, textinput.Blink
	case ":":
		m.mode = modeCommand
		m.input.SetValue("")
		m.input.Focus()
		return m, textinput.Blink
	case "?":
		m.prev = m.mode
		m.mode = modeHelp

	case "1", "2", "3", "4", "5":
		t := index.Tab(int(k[0] - '1'))
		if int(t) < len(index.TabNames) {
			m.tab = t
			m.cursor, m.offset = 0, 0
			m.rebuildView()
		}
	case "r":
		m.loading = true
		return m, m.loadIndex
	}

	if m.visual {
		// keep the anchor visible while extending
		m.clampOffset()
	}
	return m, nil
}

func (m *Model) rangeBounds() (int, int) {
	lo, hi := m.anchor, m.cursor
	if lo > hi {
		lo, hi = hi, lo
	}
	return max(0, lo), min(len(m.view.Files)-1, hi)
}

func (m *Model) toggleRange() {
	lo, hi := m.rangeBounds()
	for i := lo; i <= hi; i++ {
		m.man.Toggle(m.view.Files[i])
	}
}

func (m *Model) addRange() {
	lo, hi := m.rangeBounds()
	for i := lo; i <= hi; i++ {
		m.man.Add(m.view.Files[i])
	}
}

func (m Model) handleSelectionKey(k string) (tea.Model, tea.Cmd) {
	files := m.man.Files()
	switch k {
	case "esc", "q", "s":
		m.mode = modeNormal
	case "j", "down":
		if m.selCursor < len(files)-1 {
			m.selCursor++
		}
	case "k", "up":
		if m.selCursor > 0 {
			m.selCursor--
		}
	case "g":
		m.selCursor = 0
	case "G":
		m.selCursor = max(0, len(files)-1)
	case "d", "x":
		if m.selCursor < len(files) {
			m.man.Remove(files[m.selCursor])
			if m.selCursor >= m.man.Len() {
				m.selCursor = max(0, m.man.Len()-1)
			}
		}
	case "c":
		m.man.Clear()
		m.selCursor = 0
	case "p":
		m.mode = modeNormal
		return m.startTransfer()
	}
	h := max(1, m.h-8)
	if m.selCursor < m.selOffset {
		m.selOffset = m.selCursor
	}
	if m.selCursor >= m.selOffset+h {
		m.selOffset = m.selCursor - h + 1
	}
	return m, nil
}

func (m Model) runCommand(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return m, nil
	}
	arg := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	switch fields[0] {
	case "q":
		if m.man.Len() > 0 {
			return m, m.setStatus(fmt.Sprintf("%d selected — :q! to discard", m.man.Len()))
		}
		m.quit = true
		return m, tea.Quit
	case "q!":
		m.man.Clear()
		_ = m.man.Save(session.ManifestPath())
		m.quit = true
		return m, tea.Quit
	case "wq":
		_ = m.man.Save(session.ManifestPath())
		m.quit = true
		return m, tea.Quit
	case "dest":
		if arg == "" {
			return m, m.setStatus("destination: " + m.cfg.Dest)
		}
		p := expandHome(arg)
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			return m, m.setStatus(errStyle.Render("not a directory: " + p))
		}
		m.cfg.NoteRecent(p)
		_ = m.cfg.Save()
		return m, m.setStatus("destination: " + p)
	case "mkdir":
		if arg == "" {
			return m, m.setStatus(errStyle.Render(":mkdir needs a name"))
		}
		p := expandHome(arg)
		if !strings.HasPrefix(p, "/") {
			p = m.cfg.Dest + "/" + p
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return m, m.setStatus(errStyle.Render(err.Error()))
		}
		return m, m.setStatus("created " + p)
	case "sort":
		switch arg {
		case "date":
			m.sortBy = index.SortDate
		case "name":
			m.sortBy = index.SortName
		case "size":
			m.sortBy = index.SortSize
		default:
			return m, m.setStatus(errStyle.Render("sort: date|name|size"))
		}
		m.rebuildView()
		return m, m.setStatus("sorted by " + index.SortNames[m.sortBy])
	case "policy":
		switch arg {
		case "skip":
			m.pol = transfer.Skip
		case "overwrite":
			m.pol = transfer.Overwrite
		case "rename":
			m.pol = transfer.Rename
		default:
			return m, m.setStatus(errStyle.Render("policy: skip|overwrite|rename"))
		}
		return m, m.setStatus("collision policy: " + transfer.PolicyNames[m.pol])
	case "clear":
		m.man.Clear()
		return m, m.setStatus("selection cleared")
	case "filter":
		m.query = arg
		m.input.SetValue(arg)
		m.cursor, m.offset = 0, 0
		m.rebuildView()
		return m, nil
	case "refresh":
		m.loading = true
		return m, m.loadIndex
	case "pull":
		return m.startTransfer()
	}
	return m, m.setStatus(errStyle.Render("unknown command: " + fields[0]))
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return home + p[1:]
	}
	return p
}

func (m Model) startTransfer() (tea.Model, tea.Cmd) {
	files := m.man.Files()
	if len(files) == 0 {
		return m, m.setStatus("nothing selected")
	}
	if st, err := os.Stat(m.cfg.Dest); err != nil || !st.IsDir() {
		return m, m.setStatus(errStyle.Render("destination is not a directory: " + m.cfg.Dest))
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.tcancel = cancel
	m.tfiles = files
	m.tprog = map[int]int{}
	m.tdone = 0
	m.tfailed = nil
	m.tcomplete = false
	m.tstarted = time.Now()
	m.tevents = make(chan transfer.Event, 64)
	m.mode = modeTransfer

	dev, dest, pol, ch := m.dev, m.cfg.Dest, m.pol, m.tevents
	go func() {
		_ = transfer.Run(ctx, dev, files, dest, pol, ch)
	}()
	return m, waitEvent(m.tevents)
}
