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
	"github.com/CExSDixit/placer/internal/player"
	"github.com/CExSDixit/placer/internal/preview"
	"github.com/CExSDixit/placer/internal/session"
	"github.com/CExSDixit/placer/internal/transfer"
)

// previewDebounce is how long the cursor must rest before a preview fetch
// starts — measured and specced: long enough that fast j/k scrolling never
// triggers a fetch per row, short enough to feel immediate once it stops.
// Audio autoplay rides the same debounce: scrubbing down a list of voice
// memos with j/k must not spawn a pull and a process per row.
const previewDebounce = 120 * time.Millisecond

// playTickInterval is how often the playhead repaints while audio plays.
const playTickInterval = 500 * time.Millisecond

// maxCmdHistory caps the session's `:` history. Deep enough to arrow back to
// anything from this sitting, shallow enough that it stays a list you can
// scan rather than a log.
const maxCmdHistory = 100

// Seek steps, from the phase 1 normal-mode key table.
const (
	seekSmall = 5 * time.Second
	seekLarge = 30 * time.Second
)

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

	// command-mode history, session-only. cmdHistIdx == len(cmdHistory) means
	// "on the line being typed", which cmdDraft holds while arrowing back.
	cmdHistory []string
	cmdHistIdx int
	cmdDraft   string

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

	// audio playback. playGen invalidates an in-flight pull the same way
	// moveSeq invalidates an in-flight preview fetch: a pull that finishes
	// after the user has moved on must not start playing.
	player     *player.Player
	playGen    int
	playErr    string
	playLoad   string // name of the file being pulled before playback, "" if none
	playCancel context.CancelFunc
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
		player:  player.New(preview.Tool),
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
// again during the wait and this fetch must not start. fetch and audio say
// which of the two debounced actions the rest was scheduled for — they are
// independent (audio autoplay works with the preview pane off, and a video
// preview happens with audio autoplay off).
type previewDebounceMsg struct {
	seq          int
	path         string
	fetch, audio bool
}

// audioReadyMsg carries a pulled audio file back for playback. A stale gen
// means the cursor moved on, or the user stopped playback, while adb was
// still copying.
type audioReadyMsg struct {
	gen   int
	file  device.File
	local string
	at    time.Duration
	err   error
}

// playTickMsg repaints the playhead while audio is playing.
type playTickMsg struct{ gen int }

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

// mediaMetaRows is how much of the pane the audio/video metadata card claims.
// A photo gets the whole pane — its filename and size are already in the list
// row — but "which take is this" for a video, and codec/bitrate/playhead for
// audio, are worth more than the extra image rows.
const mediaMetaRows = 9

// previewCellSizeFor is previewCellSize with room reserved for the metadata
// card that audio and video previews render beneath the image. It is what
// sizes the fetch, so the cached render already fits — the cache key includes
// the geometry, so the two must not disagree.
func (m *Model) previewCellSizeFor(f device.File) (int, int) {
	w, h := m.previewCellSize()
	switch f.Kind() {
	case device.KindVideo, device.KindAudio:
		if h-mediaMetaRows > 4 {
			h -= mediaMetaRows
		} else {
			h = max(4, h/2)
		}
	}
	return w, h
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

	f, ok := m.cur()
	if !ok || m.dev == nil {
		m.preview = previewState{}
		return nil
	}
	path := previewKeyFor(f)

	cellW, _ := m.previewCellSize()
	fetch := m.cfg.Preview && cellW > 0

	// Video is gated on autoplay, which defaults off: a frame grab costs
	// ~1.2 s even with the sparse head+tail trick, so firing one on every
	// cursor move would fight j/k. Off means a metadata card, built from the
	// MediaStore row with no device round trip at all — exactly how heic has
	// behaved since phase 2.
	if fetch && f.Kind() == device.KindVideo && !m.cfg.Autoplay {
		m.preview = previewState{path: path,
			result: preview.MetaCard(f, "video — :set autoplay on to grab a frame")}
		fetch = false
	}

	// The scrub-through-voice-memos flow: j/k in the Audio tab moves the
	// cursor and plays what it lands on. Independent of the preview pane, so
	// it still works with `:set preview off`.
	audio := m.cfg.Audio && f.Kind() == device.KindAudio && m.player.Available()
	if audio {
		// Whatever was playing belongs to the row we just left.
		m.stopPlayback()
	}

	if !fetch && !audio {
		if m.preview.path != path {
			m.preview = previewState{}
		}
		return nil
	}
	if fetch {
		m.preview = previewState{path: path, loading: true}
	}
	return tea.Tick(previewDebounce, func(time.Time) tea.Msg {
		return previewDebounceMsg{seq: seq, path: path, fetch: fetch, audio: audio}
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

// stopPlayback kills any playing process and retires any in-flight pull.
// Bumping playGen is what makes a pull that is already running land as a
// stale audioReadyMsg instead of starting playback nobody asked for.
func (m *Model) stopPlayback() {
	m.playGen++
	if m.playCancel != nil {
		m.playCancel()
		m.playCancel = nil
	}
	m.playLoad = ""
	m.player.Stop()
}

// shutdown releases everything that outlives the TUI process otherwise: an
// `adb pull` running for a preview, and an ffplay process still making noise.
func (m *Model) shutdown() {
	m.cancelPreview()
	m.stopPlayback()
}

// loadAndPlay pulls f into the media cache (a no-op when the preview fetch
// already cached it, which is the common case) and hands it to the player.
func (m *Model) loadAndPlay(f device.File, gen int, at time.Duration) tea.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	m.playCancel = cancel
	m.playLoad = f.Name
	m.playErr = ""
	dev := m.dev
	return func() tea.Msg {
		local, err := preview.EnsureLocal(ctx, dev, f)
		return audioReadyMsg{gen: gen, file: f, local: local, at: at, err: err}
	}
}

func playTick(gen int) tea.Cmd {
	return tea.Tick(playTickInterval, func(time.Time) tea.Msg { return playTickMsg{gen: gen} })
}

// playCurrent starts (or restarts) playback of the file under the cursor.
func (m *Model) playCurrent(at time.Duration) tea.Cmd {
	f, ok := m.cur()
	if !ok || !m.player.Available() {
		return nil
	}
	m.stopPlayback()
	return m.loadAndPlay(f, m.playGen, at)
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
		var cmds []tea.Cmd
		if msg.audio {
			m.playGen++
			cmds = append(cmds, m.loadAndPlay(f, m.playGen, 0))
		}
		if msg.fetch {
			cellW, cellH := m.previewCellSizeFor(f)
			if cellW > 0 {
				ctx, cancel := context.WithCancel(context.Background())
				m.previewCancel = cancel
				dev, proto, seq := m.dev, m.proto, msg.seq
				cmds = append(cmds, func() tea.Msg {
					res, err := preview.Fetch(ctx, dev, f, cellW, cellH, proto)
					return previewResultMsg{seq: seq, result: res, err: err}
				})
			}
		}
		return m, tea.Batch(cmds...)

	case previewResultMsg:
		if msg.seq != m.moveSeq {
			return m, nil // stale — the cursor has since moved on
		}
		m.previewCancel = nil
		m.preview.loading = false
		m.preview.result = msg.result
		m.preview.err = msg.err
		return m, nil

	case audioReadyMsg:
		if msg.gen != m.playGen {
			return m, nil // stale — playback was stopped or moved on mid-pull
		}
		m.playCancel = nil
		m.playLoad = ""
		if msg.err != nil {
			m.playErr = msg.err.Error()
			return m, nil
		}
		// ffprobe's container duration beats MediaStore's when the preview
		// pane already measured one — the playhead and the end-of-track stop
		// both key off it.
		dur := msg.file.Duration
		if r := m.preview.result; r.Duration > 0 && m.preview.path == previewKeyFor(msg.file) {
			dur = r.Duration
		}
		m.player.Play(previewKeyFor(msg.file), msg.file.Name, msg.local, dur, msg.at)
		return m, playTick(msg.gen)

	case playTickMsg:
		if msg.gen != m.playGen {
			return m, nil
		}
		if m.player.Playing() {
			return m, playTick(msg.gen)
		}
		// One final tick after the process exits, so the footer repaints from
		// "playing" to "paused" without waiting for the next keystroke.
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
			m.pushHistory(cmdline)
			m.cmdHistIdx = len(m.cmdHistory)
			return m.runCommand(cmdline)
		case "tab":
			line, hint := m.completeCommand(m.input.Value())
			m.input.SetValue(line)
			m.input.CursorEnd()
			if hint != "" {
				return m, m.setStatus(hint)
			}
			return m, nil
		case "up":
			m.historyStep(-1)
			return m, nil
		case "down":
			m.historyStep(1)
			return m, nil
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

	// Transport keys live in the Audio tab only. Elsewhere they stay free for
	// list navigation, and space keeps meaning "select" — the binding it has
	// had since phase 1 everywhere else in the app.
	if m.tab == index.TabAudio && m.player.Available() {
		if handled, cmd := m.handleTransportKey(k); handled {
			return m, cmd
		}
	}

	n := len(m.view.Files)

	switch k {
	case "q":
		if m.man.Len() > 0 {
			return m, m.setStatus(fmt.Sprintf("%d file(s) selected — :q! to discard, p to pull, s to review", m.man.Len()))
		}
		m.shutdown()
		m.quit = true
		return m, tea.Quit
	case "ctrl+c":
		_ = m.man.Save(session.ManifestPath())
		m.shutdown()
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
		m.cmdHistIdx = len(m.cmdHistory)
		m.cmdDraft = ""
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

// handleTransportKey implements the Audio tab's playback controls. It reports
// whether it consumed the key, so anything it doesn't claim falls through to
// the normal-mode keymap unchanged.
//
// The playhead lives here, not in the player process: pause kills ffplay and
// remembers the offset, seek kills it and restarts with a new -ss. Crude, but
// it needs no platform-specific code and no pure-Go decoder — which would
// have meant CGO, which placer does not do.
func (m *Model) handleTransportKey(k string) (bool, tea.Cmd) {
	switch k {
	case " ":
		f, ok := m.cur()
		if !ok {
			return true, nil
		}
		// Space on a different file loads it; space on the loaded one is
		// play/pause.
		if loaded, _ := m.player.Loaded(); loaded != previewKeyFor(f) {
			return true, m.playCurrent(0)
		}
		if m.player.Toggle() {
			return true, playTick(m.playGen)
		}
		return true, nil

	case "h", "left":
		m.player.Seek(-seekSmall)
	case "l", "right":
		m.player.Seek(seekSmall)
	case "H":
		m.player.Seek(-seekLarge)
	case "L":
		m.player.Seek(seekLarge)
	case "[":
		return true, m.setStatus(fmt.Sprintf("speed %.2fx", m.player.AdjustSpeed(-player.SpeedStep)))
	case "]":
		return true, m.setStatus(fmt.Sprintf("speed %.2fx", m.player.AdjustSpeed(player.SpeedStep)))
	default:
		return false, nil
	}
	if m.player.Playing() {
		return true, playTick(m.playGen)
	}
	return true, nil
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
		m.shutdown()
		m.quit = true
		return m, tea.Quit
	case "q!":
		m.man.Clear()
		_ = m.man.Save(session.ManifestPath())
		m.shutdown()
		m.quit = true
		return m, tea.Quit
	case "wq":
		_ = m.man.Save(session.ManifestPath())
		m.shutdown()
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

	// Every toggle takes effect on the file already under the cursor, without
	// waiting for a move: `:set autoplay on` should grab the frame for the
	// video you are looking at, and `:set audio off` should stop the memo
	// currently playing.
	var pcmd tea.Cmd
	switch {
	case fields[0] == "audio" && !on:
		m.stopPlayback()
	case !on && fields[0] == "preview":
		m.preview = previewState{}
	default:
		pcmd = m.schedulePreview()
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
