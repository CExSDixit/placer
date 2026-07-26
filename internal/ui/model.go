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
	"github.com/CExSDixit/placer/internal/preview"
	"github.com/CExSDixit/placer/internal/session"
	"github.com/CExSDixit/placer/internal/transfer"
)

// previewDebounce is how long the cursor must rest before a preview fetch
// starts — measured and specced: long enough that fast j/k scrolling never
// triggers a fetch per row, short enough to feel immediate once it stops.
const previewDebounce = 120 * time.Millisecond

// previewState is what the preview pane renders. path identifies which file
// it belongs to, so a stale in-flight result can never paint over the file
// the cursor has since moved to.
type previewState struct {
	path    string
	loading bool
	result  preview.Result
	err     error
}

type mode int

const (
	modeNormal mode = iota
	modeSearch
	modeCommand
	modeDest
	modeSelection
	modeTransfer
	modeHelp
	modeBuckets
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
	bucket string // album/folder filter, exact match; "" = no filter
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

	// bucket browser (`:buckets`)
	bucketList                         []index.BucketCount
	bucketListCursor, bucketListOffset int

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

	// preview pipeline: debounced on cursor rest, cancelled on cursor move.
	proto         preview.Protocol
	preview       previewState
	moveSeq       int
	previewCancel context.CancelFunc
}

func New(dev device.Device, proto preview.Protocol) Model {
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
		proto:   proto,
	}
}

type indexLoadedMsg struct {
	ix   *index.Index
	errs []error
}
type statusExpiredMsg struct{}
type transferEventMsg transfer.Event
type transferDoneMsg struct{}

// previewDebounceMsg fires once the cursor has rested for previewDebounce.
// If seq no longer matches the model's current moveSeq, the cursor moved
// again during the wait and this fetch must not start.
type previewDebounceMsg struct {
	seq  int
	path string
}

// previewResultMsg carries a completed fetch back to Update. A stale seq
// means the cursor moved on before this finished; it's discarded rather than
// painted over whatever the pane is now showing.
type previewResultMsg struct {
	seq    int
	result preview.Result
	err    error
}

func previewKeyFor(f device.File) string {
	if f.Path != "" {
		return f.Path
	}
	return f.ContentURI()
}

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
	m.view = m.ix.Build(m.tab, m.bucket, m.query, m.sortBy)
	if m.cursor >= len(m.view.Files) {
		m.cursor = max(0, len(m.view.Files)-1)
	}
	m.clampOffset()
}

func (m *Model) listHeight() int {
	// header(2) + column head(1) + footer(2)
	return max(1, m.h-6)
}

// minPreviewPaneW/H below which a preview pane isn't worth showing — mostly
// metadata cards fit but images turn to a smear of a handful of characters.
const (
	minPreviewPaneW     = 24
	minTotalWForPreview = 80
)

// previewPaneWidth returns 0 when there isn't room for a preview pane (too
// narrow a terminal, or the user turned previews off), otherwise the column
// width to reserve on the right of the file list.
func (m *Model) previewPaneWidth() int {
	if !m.cfg.Preview || m.mode != modeNormal || m.w < minTotalWForPreview {
		return 0
	}
	pw := m.w / 3
	if pw > 48 {
		pw = 48
	}
	if pw < minPreviewPaneW || m.w-pw-1 < 40 {
		return 0
	}
	return pw
}

// previewCellSize is the pane's content area in terminal cells, used both to
// size the fetch/render request and as part of the cache key.
func (m *Model) previewCellSize() (int, int) {
	pw := m.previewPaneWidth()
	return pw, m.listHeight()
}

// schedulePreview bumps moveSeq (invalidating any in-flight fetch) and
// cancels it, then — if a file is under the cursor and previews are on —
// schedules a debounced fetch for it. Called whenever a key could have
// changed which file the cursor is resting on.
func (m *Model) schedulePreview() tea.Cmd {
	m.moveSeq++
	seq := m.moveSeq
	if m.previewCancel != nil {
		m.previewCancel()
		m.previewCancel = nil
	}

	cellW, _ := m.previewCellSize()
	f, ok := m.cur()
	if !m.cfg.Preview || cellW == 0 || !ok || m.dev == nil {
		m.preview = previewState{}
		return nil
	}

	path := previewKeyFor(f)
	m.preview = previewState{path: path, loading: true}
	return tea.Tick(previewDebounce, func(time.Time) tea.Msg {
		return previewDebounceMsg{seq: seq, path: path}
	})
}

// cancelPreview stops any in-flight preview fetch — called on quit so an
// `adb pull` for a preview doesn't keep running past the TUI exiting.
func (m *Model) cancelPreview() {
	if m.previewCancel != nil {
		m.previewCancel()
		m.previewCancel = nil
	}
}

// curPreviewKey identifies the file the preview pane should be showing right
// now, or "" if none — used to detect whether a key press actually moved the
// cursor onto a different file.
func (m Model) curPreviewKey() string {
	f, ok := m.cur()
	if !ok {
		return ""
	}
	return previewKeyFor(f)
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
		pcmd := m.schedulePreview()
		return m, tea.Batch(pcmd, m.setStatus(fmt.Sprintf("indexed %d files (%d photos, %d video, %d audio)",
			len(m.ix.All), c[index.TabPhotos], c[index.TabVideo], c[index.TabAudio])))

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

	case previewDebounceMsg:
		if msg.seq != m.moveSeq {
			return m, nil // cursor moved again during the debounce window
		}
		f, ok := m.cur()
		if !ok || previewKeyFor(f) != msg.path {
			return m, nil
		}
		cellW, cellH := m.previewCellSize()
		if cellW == 0 {
			return m, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.previewCancel = cancel
		dev, proto, seq := m.dev, m.proto, msg.seq
		return m, func() tea.Msg {
			res, err := preview.Fetch(ctx, dev, f, cellW, cellH, proto)
			return previewResultMsg{seq: seq, result: res, err: err}
		}

	case previewResultMsg:
		if msg.seq != m.moveSeq {
			return m, nil // stale — the cursor has since moved on
		}
		m.previewCancel = nil
		m.preview.loading = false
		m.preview.result = msg.result
		m.preview.err = msg.err
		return m, nil
	}

	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	before := m.curPreviewKey()
	beforeBucket, beforeQuery, beforeTab := m.bucket, m.query, m.tab
	newModel, cmd := m.handleKey(km)
	nm := newModel.(Model)
	if nm.curPreviewKey() != before || nm.bucket != beforeBucket || nm.query != beforeQuery || nm.tab != beforeTab {
		pcmd := nm.schedulePreview()
		return nm, tea.Batch(cmd, pcmd)
	}
	return nm, cmd
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

	case modeBuckets:
		return m.handleBucketsKey(k)

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
		m.cancelPreview()
		m.quit = true
		return m, tea.Quit
	case "ctrl+c":
		_ = m.man.Save(session.ManifestPath())
		m.cancelPreview()
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
	case "v", "V":
		// Anchor a range at the cursor; j/k extends it, any select key
		// commits (vim line-visual). "V" is the primary binding — real vim
		// convention for linewise visual — with lowercase "v" kept for
		// phase 1 compatibility.
		//
		// shift+space was the original ask, but it cannot be a *binding* at
		// all under bubbletea 1.3.10: ASCII space is 0x20 whether or not
		// shift is held, and telling them apart needs the terminal's kitty
		// keyboard protocol enabled and bubbletea to parse the resulting
		// CSI-u sequence — a feature this bubbletea version doesn't have.
		// Rather than claim a binding that silently never fires, it's
		// omitted; V/*/ctrl+a/ctrl+x below are the reliable route on every
		// terminal, including Terminal.app.
		m.visual = !m.visual
		m.anchor = m.cursor
	case "ctrl+a":
		for _, f := range m.view.Files {
			m.man.Add(f)
		}
		return m, m.setStatus(fmt.Sprintf("selected all %d visible (%d total)", len(m.view.Files), m.man.Len()))
	case "*":
		// Selects everything matching the current tab+filter — the bulk
		// workflow the real library demands: WhatsApp Images (5,167) dwarfs
		// Camera (2,186), so "filter to Camera, press *" is how curation
		// actually happens. Filtering already narrows m.view, so this is the
		// same operation as ctrl+a under a more discoverable key.
		for _, f := range m.view.Files {
			m.man.Add(f)
		}
		return m, m.setStatus(fmt.Sprintf("selected all %d matching (%d total)", len(m.view.Files), m.man.Len()))
	case "ctrl+x":
		for _, f := range m.view.Files {
			m.man.Remove(f)
		}
		return m, m.setStatus(fmt.Sprintf("cleared %d visible (%d remain)", len(m.view.Files), m.man.Len()))
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
	case "B":
		// Quick Camera-only toggle. WhatsApp Images (5,167) dwarfs Camera
		// (2,186) on the reference library, so the Photos tab is mostly
		// WhatsApp — this is the fastest way back to "my own photos".
		if m.bucket == "" {
			m.bucket = "Camera"
		} else {
			m.bucket = ""
		}
		m.cursor, m.offset = 0, 0
		m.rebuildView()
		if m.bucket != "" {
			return m, m.setStatus("bucket: " + m.bucket)
		}
		return m, m.setStatus("bucket filter cleared")
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

// handleBucketsKey drives the `:buckets` browser: j/k to move, enter/tab to
// filter the current tab down to that bucket, "c" to clear any bucket
// filter, esc/q to leave without changing anything.
func (m Model) handleBucketsKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "esc", "q":
		m.mode = modeNormal
	case "j", "down":
		if m.bucketListCursor < len(m.bucketList)-1 {
			m.bucketListCursor++
		}
	case "k", "up":
		if m.bucketListCursor > 0 {
			m.bucketListCursor--
		}
	case "g":
		m.bucketListCursor = 0
	case "G":
		m.bucketListCursor = max(0, len(m.bucketList)-1)
	case "enter", "tab":
		if m.bucketListCursor < len(m.bucketList) {
			name := m.bucketList[m.bucketListCursor].Name
			if name == "(none)" {
				name = ""
			}
			m.bucket = name
		}
		m.mode = modeNormal
		m.cursor, m.offset = 0, 0
		m.rebuildView()
		return m, m.setStatus(fmt.Sprintf("bucket: %s (%d files)", m.bucket, len(m.view.Files)))
	case "c":
		m.bucket = ""
		m.mode = modeNormal
		m.cursor, m.offset = 0, 0
		m.rebuildView()
		return m, m.setStatus("bucket filter cleared")
	}
	h := max(1, m.h-8)
	if m.bucketListCursor < m.bucketListOffset {
		m.bucketListOffset = m.bucketListCursor
	}
	if m.bucketListCursor >= m.bucketListOffset+h {
		m.bucketListOffset = m.bucketListCursor - h + 1
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
		m.cancelPreview()
		m.quit = true
		return m, tea.Quit
	case "q!":
		m.man.Clear()
		_ = m.man.Save(session.ManifestPath())
		m.cancelPreview()
		m.quit = true
		return m, tea.Quit
	case "wq":
		_ = m.man.Save(session.ManifestPath())
		m.cancelPreview()
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
	case "bucket":
		if arg == "" {
			if m.bucket == "" {
				return m, m.setStatus("no bucket filter")
			}
			return m, m.setStatus("bucket: " + m.bucket)
		}
		if arg == "clear" {
			m.bucket = ""
		} else {
			m.bucket = arg
		}
		m.cursor, m.offset = 0, 0
		m.rebuildView()
		if m.bucket == "" {
			return m, m.setStatus("bucket filter cleared")
		}
		return m, m.setStatus(fmt.Sprintf("bucket: %s (%d files)", m.bucket, len(m.view.Files)))
	case "buckets":
		if m.ix == nil {
			return m, m.setStatus(errStyle.Render("still indexing"))
		}
		m.bucketList = m.ix.Buckets(m.tab)
		m.bucketListCursor, m.bucketListOffset = 0, 0
		m.mode = modeBuckets
		return m, nil
	case "set":
		return m.runSet(arg)
	case "refresh":
		m.loading = true
		return m, m.loadIndex
	case "pull":
		return m.startTransfer()
	}
	return m, m.setStatus(errStyle.Render("unknown command: " + fields[0]))
}

// runSet handles `:set <toggle> on|off`. Every preview toggle is independent
// and switchable at runtime, persisted to config.json, so nobody ever has to
// hold a key down to stop something playing.
func (m Model) runSet(arg string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(arg)
	if len(fields) != 2 || (fields[1] != "on" && fields[1] != "off") {
		return m, m.setStatus(errStyle.Render("usage: :set preview|autoplay|audio on|off"))
	}
	on := fields[1] == "on"
	switch fields[0] {
	case "preview":
		m.cfg.Preview = on
	case "autoplay":
		m.cfg.Autoplay = on
	case "audio":
		m.cfg.Audio = on
	default:
		return m, m.setStatus(errStyle.Render("unknown setting: " + fields[0]))
	}
	_ = m.cfg.Save()
	var pcmd tea.Cmd
	if fields[0] == "preview" {
		if on {
			pcmd = m.schedulePreview()
		} else {
			m.preview = previewState{}
		}
	}
	return m, tea.Batch(pcmd, m.setStatus(fmt.Sprintf("%s: %s", fields[0], fields[1])))
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
