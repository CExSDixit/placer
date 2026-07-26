package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/index"
	"github.com/CExSDixit/placer/internal/preview"
	"github.com/CExSDixit/placer/internal/transfer"
)

func (m Model) View() string {
	if m.quit {
		return ""
	}
	switch m.mode {
	case modeHelp:
		return m.helpView()
	case modeDest:
		return m.header() + "\n" + m.dest.View(m.w) + "\n" + m.footer()
	case modeSelection:
		return m.header() + "\n" + m.selectionView() + "\n" + m.footer()
	case modeTransfer:
		return m.header() + "\n" + m.transferView() + "\n" + m.footer()
	case modeBuckets:
		return m.header() + "\n" + m.bucketsView() + "\n" + m.footer()
	}
	frame := m.header() + "\n" + m.bodyView() + "\n" + m.footer()
	return frame + m.graphicsOverlay()
}

// bodyView composes the file list with a preview pane on the right, when
// there's room and previews are on. It builds the two blocks independently
// and zips them by raw line, rather than via lipgloss.JoinHorizontal:
// lipgloss.Width strips ordinary SGR color codes but doesn't understand the
// Kitty/iTerm graphics-protocol escapes the preview pane can contain, so
// running it over those bytes would badly miscount their "visible" width and
// corrupt the whole layout.
func (m Model) bodyView() string {
	pw := m.previewPaneWidth()
	if pw <= 0 {
		return m.listViewWidth(m.w)
	}
	listW := m.w - pw - 1
	listLines := strings.Split(m.listViewWidth(listW), "\n")
	paneLines := strings.Split(m.previewPaneView(pw, len(listLines)), "\n")

	var b strings.Builder
	for i, line := range listLines {
		b.WriteString(padVisible(line, listW))
		b.WriteString(" ")
		if i < len(paneLines) {
			b.WriteString(paneLines[i])
		}
		if i < len(listLines)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func padVisible(s string, w int) string {
	if vis := lipgloss.Width(s); vis < w {
		return s + strings.Repeat(" ", w-vis)
	}
	return s
}

// previewPaneView renders the right-hand pane's plain-text content: a title
// row plus, below it, either the half-block image (which is itself ordinary
// printable characters and fits fine here) or a status/metadata line.
//
// Kitty/iTerm/sixel images are deliberately NOT written into this string —
// see graphicsOverlay — because those protocols' escape sequences aren't
// ordinary printable content and would break every width calculation this
// function and bodyView rely on if mixed in.
func (m Model) previewPaneView(pw, height int) string {
	lines := make([]string, height)
	title := "preview"
	if f, ok := m.cur(); ok {
		title = f.Name
	}
	lines[0] = dimStyle.Render(pad(trunc(title, pw), pw))
	if height < 2 {
		return strings.Join(lines, "\n")
	}

	// write puts plain-text rows into the pane starting at row `from`,
	// stopping at the pane's height.
	write := func(from int, rows []string) int {
		for _, r := range rows {
			if from >= height {
				break
			}
			lines[from] = r
			from++
		}
		return from
	}
	fill := func(s string) {
		for i := 1; i < height; i++ {
			lines[i] = ""
		}
		lines[1] = s
	}
	metaRows := func() []string {
		out := make([]string, 0, len(m.preview.result.Meta))
		for _, l := range m.preview.result.Meta {
			out = append(out, dimStyle.Render(pad(trunc(l, pw), pw)))
		}
		return out
	}

	res := m.preview.result
	switch {
	case m.preview.loading:
		fill(dimStyle.Render(pad("loading…", pw)))
	case m.preview.err != nil:
		fill(errStyle.Render(pad(trunc(m.preview.err.Error(), pw), pw)))

	case res.Tier == preview.TierMeta:
		// The metadata card: whatever MediaStore knows, plus the reason there
		// is no image — heic, ffmpeg missing, or video autoplay off.
		for i := 1; i < height; i++ {
			lines[i] = ""
		}
		row := 1
		if note := res.Note; note != "" {
			row = write(row, []string{dimStyle.Render(pad(trunc(note, pw), pw)), ""})
		}
		write(row, metaRows())

	case len(res.Rendered) == 0:
		fill(dimStyle.Render(pad("no preview", pw)))

	case m.proto == preview.ProtoHalfBlock:
		for i := 1; i < height; i++ {
			lines[i] = ""
		}
		imgLines := strings.Split(string(res.Rendered), "\n")
		row := write(1, imgLines)
		// Audio and video previews carry a metadata card under the image;
		// there is more worth saying about them than about a photo.
		if len(res.Meta) > 0 && row+1 < height {
			write(row+1, metaRows())
		}

	default:
		// The image itself is placed via graphicsOverlay at the same row/col
		// this line would occupy; left blank here so we don't print ordinary
		// text over where the terminal is about to draw it. Metadata still
		// goes in as text, below the space the image will occupy.
		for i := 1; i < height; i++ {
			lines[i] = ""
		}
		if len(res.Meta) > 0 {
			row := 1 + m.overlayRows()
			if row+1 < height {
				write(row+1, metaRows())
			}
		}
	}
	return strings.Join(lines, "\n")
}

// overlayRows is how many character rows a graphics-protocol image occupies,
// so the metadata card lands beneath it rather than underneath it. It must
// agree with the cellH the fetch was sized at, which is why both go through
// previewCellSizeFor.
func (m Model) overlayRows() int {
	f, ok := m.cur()
	if !ok {
		return m.listHeight()
	}
	_, h := m.previewCellSizeFor(f)
	return h
}

// graphicsOverlay emits a Kitty/iTerm/sixel image via absolute cursor
// positioning, appended after the rest of the frame. The body is laid out by
// bodyView first with the pane's image row left blank (see
// previewPaneView), then this moves the cursor to that exact cell, draws the
// image, and restores the cursor — the same reserve-then-place technique
// terminal file managers (yazi, ranger, lf) use for Kitty previews in a
// line-based TUI.
func (m Model) graphicsOverlay() string {
	if m.proto == preview.ProtoHalfBlock {
		return ""
	}
	pw := m.previewPaneWidth()
	if pw <= 0 {
		return ""
	}
	res := m.preview.result
	if m.preview.loading || m.preview.err != nil || len(res.Rendered) == 0 {
		return ""
	}
	if !res.HasImage() {
		return ""
	}
	// header() is always exactly 2 lines; bodyView's first line is the pane
	// title, so pane content starts on the 4th on-screen row. The list
	// column occupies m.w-pw-1 cells plus a 1-column separator.
	const bodyStartRow = 3
	row := bodyStartRow + 1
	col := (m.w - pw - 1) + 2
	return fmt.Sprintf("\x1b[s\x1b[%d;%dH%s\x1b[u", row, col, res.Rendered)
}

func (m Model) header() string {
	counts := map[index.Tab]int{}
	if m.ix != nil {
		counts = m.ix.Counts()
	}

	// Tab labels grow with the library size (5 digits of count each once there
	// are 10k+ files), so the strip has to shed detail on a narrow terminal
	// rather than overflow: full label -> no counts -> abbreviated -> numbers.
	labelFor := []func(i int, t index.Tab) string{
		func(i int, t index.Tab) string { return fmt.Sprintf("%d:%s(%d)", i+1, t, counts[t]) },
		func(i int, t index.Tab) string { return fmt.Sprintf("%d:%s", i+1, t) },
		func(i int, t index.Tab) string { return fmt.Sprintf("%d:%.3s", i+1, t) },
		func(i int, t index.Tab) string { return fmt.Sprint(i + 1) },
	}

	var labels []string
	for _, mk := range labelFor {
		labels = labels[:0]
		total := 0
		for i := range index.TabNames {
			l := mk(i, index.Tab(i))
			labels = append(labels, l)
			total += len([]rune(l)) + 2 // tab styles pad by 1 either side
		}
		if total <= m.w {
			break
		}
	}

	var tabs []string
	for i, l := range labels {
		if index.Tab(i) == m.tab {
			tabs = append(tabs, tabActive.Render(l))
		} else {
			tabs = append(tabs, tabInactive.Render(l))
		}
	}
	left := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)

	// The status cluster degrades rather than getting chopped mid-word by the
	// terminal: drop the policy, then the sort, then the serial, whichever
	// still fits. Tab labels grow with the library size, so on a narrow
	// terminal there may be room for none of it.
	avail := m.w - lipgloss.Width(left) - 2
	rightPlain := ""
	for _, c := range []string{
		fmt.Sprintf("%s · sort:%s · %s", m.dev.Serial(), index.SortNames[m.sortBy], transfer.PolicyNames[m.pol]),
		fmt.Sprintf("%s · sort:%s", m.dev.Serial(), index.SortNames[m.sortBy]),
		m.dev.Serial(),
		fmt.Sprintf("sort:%s", index.SortNames[m.sortBy]),
	} {
		if len([]rune(c)) <= avail {
			rightPlain = c
			break
		}
	}

	line1 := left
	if rightPlain != "" {
		gap := m.w - lipgloss.Width(left) - len([]rune(rightPlain))
		if gap < 1 {
			gap = 1
		}
		line1 += strings.Repeat(" ", gap) + headerStyle.Render(rightPlain)
	}

	sel := fmt.Sprintf("%d selected (%s)", m.man.Len(), humanBytes(m.man.TotalBytes()))
	line2 := selectedStyle.Render(sel)
	if room := m.w - len([]rune(sel)) - 6; room > 8 {
		line2 += "  " + pathStyle.Render("→ "+truncLeft(m.cfg.Dest, room))
	}

	return line1 + "\n" + line2
}

// columns sizes the list. The name column takes what is left over but is
// capped: real filenames run ~30 chars, so on a wide terminal an uncapped name
// column pushes size/date/type to the far edge and leaves a dead gap in the
// middle.
const maxNameCol = 52

func (m Model) columns() (name, size, date, kind int) {
	return m.columnsFor(m.w)
}

func (m Model) columnsFor(w int) (name, size, date, kind int) {
	size, date, kind = 8, 16, 7
	name = w - size - date - kind - 8
	if name > maxNameCol {
		name = maxNameCol
	}
	if name < 12 {
		name = 12
	}
	return
}

// listView renders the file list at the model's full width — used when no
// preview pane is showing.
func (m Model) listView() string {
	return m.listViewWidth(m.w)
}

func (m Model) listViewWidth(w int) string {
	if m.loading {
		return "\n  " + dimStyle.Render("indexing device…")
	}
	if len(m.view.Files) == 0 {
		if m.query != "" {
			return "\n  " + dimStyle.Render("no matches for "+m.query)
		}
		return "\n  " + dimStyle.Render("no files in this tab")
	}

	nameW, sizeW, dateW, kindW := m.columnsFor(w)
	var b strings.Builder
	b.WriteString(dimStyle.Render(fmt.Sprintf("   %s %s %s %s",
		pad("name", nameW), pad("size", sizeW), pad("date", dateW), pad("type", kindW))) + "\n")

	h := m.listHeight()
	end := min(len(m.view.Files), m.offset+h)
	lo, hi := -1, -1
	if m.visual {
		lo, hi = m.rangeBounds()
	}

	for i := m.offset; i < end; i++ {
		f := m.view.Files[i]

		marker := " "
		if m.man.Has(f) {
			marker = selectedStyle.Render("✓")
		}
		cursor := " "
		if i == m.cursor {
			cursor = cursorStyle.Render("▸")
		}

		name := renderName(f.Name, m.view.Matches[i], nameW)
		meta := fmt.Sprintf("%s %s %s",
			pad(humanBytes(f.Size), sizeW),
			pad(humanTime(f.SortTime()), dateW),
			pad(shortKind(f), kindW))

		line := fmt.Sprintf("%s%s %s %s", cursor, marker, name, dimStyle.Render(meta))
		if m.visual && i >= lo && i <= hi {
			line = visualStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderName highlights fuzzy-matched runes inside the name column.
func renderName(name string, matches []int, w int) string {
	truncated := trunc(name, w)
	if len(matches) == 0 {
		return pad(truncated, w)
	}
	hit := make(map[int]bool, len(matches))
	for _, i := range matches {
		hit[i] = true
	}
	var b strings.Builder
	for i, r := range truncated {
		if hit[i] {
			b.WriteString(matchStyle.Render(string(r)))
		} else {
			b.WriteRune(r)
		}
	}
	visible := len([]rune(truncated))
	if visible < w {
		b.WriteString(strings.Repeat(" ", w-visible))
	}
	return b.String()
}

// shortKind renders a compact type label: durations for timed media,
// otherwise the informative tail of the mime subtype ("image/x-adobe-dng"
// reads as "dng", "image/svg+xml" as "svg").
func shortKind(f device.File) string {
	if f.Duration > 0 {
		return humanDuration(f.Duration)
	}
	sub := f.Mime
	if i := strings.Index(sub, "/"); i >= 0 {
		sub = sub[i+1:]
	}
	if sub == "" {
		return f.Kind().String()
	}
	sub = strings.TrimPrefix(sub, "x-")
	if i := strings.Index(sub, "+"); i > 0 {
		sub = sub[:i]
	}
	if i := strings.LastIndex(sub, "-"); i >= 0 && i < len(sub)-1 {
		sub = sub[i+1:]
	}
	if i := strings.LastIndex(sub, "."); i >= 0 && i < len(sub)-1 {
		sub = sub[i+1:]
	}
	return sub
}

func (m Model) selectionView() string {
	files := m.man.Files()
	if len(files) == 0 {
		return "\n  " + dimStyle.Render("selection is empty")
	}
	nameW, sizeW, dateW, _ := m.columns()
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf(" selection · %d files · %s ", len(files), humanBytes(m.man.TotalBytes()))) + "\n")

	h := max(1, m.h-8)
	end := min(len(files), m.selOffset+h)
	for i := m.selOffset; i < end; i++ {
		f := files[i]
		cursor := " "
		if i == m.selCursor {
			cursor = cursorStyle.Render("▸")
		}
		b.WriteString(fmt.Sprintf("%s %s %s %s\n", cursor,
			pad(trunc(f.Name, nameW), nameW),
			dimStyle.Render(pad(humanBytes(f.Size), sizeW)),
			dimStyle.Render(pad(humanTime(f.SortTime()), dateW))))
	}
	b.WriteString("\n" + dimStyle.Render("j/k move · d remove · c clear all · p pull · esc back"))
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) bucketsView() string {
	if len(m.bucketList) == 0 {
		return "\n  " + dimStyle.Render("no buckets in this tab")
	}
	nameW, _, _, _ := m.columns()
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf(" buckets in %s · %d distinct ", index.TabNames[m.tab], len(m.bucketList))) + "\n")
	if m.bucket != "" {
		b.WriteString(dimStyle.Render("current filter: "+m.bucket) + "\n")
	}

	h := max(1, m.h-9)
	end := min(len(m.bucketList), m.bucketListOffset+h)
	for i := m.bucketListOffset; i < end; i++ {
		bk := m.bucketList[i]
		cursor := " "
		if i == m.bucketListCursor {
			cursor = cursorStyle.Render("▸")
		}
		marker := " "
		if m.bucket != "" && bk.Name == m.bucket {
			marker = selectedStyle.Render("✓")
		}
		b.WriteString(fmt.Sprintf("%s%s %s %s\n", cursor, marker,
			pad(trunc(bk.Name, nameW), nameW),
			dimStyle.Render(fmt.Sprint(bk.Count))))
	}
	b.WriteString("\n" + dimStyle.Render("j/k move · enter/tab filter to this bucket · c clear filter · esc back"))
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) transferView() string {
	var b strings.Builder
	title := fmt.Sprintf(" transferring %d files → %s ", len(m.tfiles), m.cfg.Dest)
	b.WriteString(titleStyle.Render(title) + "\n\n")

	done := m.tdone
	total := len(m.tfiles)
	pct := 0
	if total > 0 {
		pct = done * 100 / total
	}
	b.WriteString(fmt.Sprintf("  %s  %d/%d files\n\n", bar(pct, min(40, m.w-24)), done, total))

	// Show the files currently in flight, plus recent failures.
	shown := 0
	for i, f := range m.tfiles {
		p, active := m.tprog[i]
		if !active || p >= 100 || shown >= 4 {
			continue
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", bar(p, 20), trunc(f.Name, max(10, m.w-30))))
		shown++
	}

	if len(m.tfailed) > 0 {
		b.WriteString("\n" + errStyle.Render(fmt.Sprintf("  %d failed:", len(m.tfailed))) + "\n")
		for i, ev := range m.tfailed {
			if i >= 3 {
				b.WriteString(dimStyle.Render(fmt.Sprintf("    …and %d more\n", len(m.tfailed)-3)))
				break
			}
			b.WriteString(errStyle.Render(fmt.Sprintf("    %s: %v\n", trunc(ev.File.Name, 30), ev.Err)))
		}
	}

	if m.tcomplete {
		ok := total - len(m.tfailed)
		msg := fmt.Sprintf("  done — %d pulled, %d failed in %s", ok, len(m.tfailed), m.telapsed.Round(time.Millisecond*100))
		b.WriteString("\n" + okStyle.Render(msg) + "\n")
		b.WriteString(dimStyle.Render("  successful files cleared from selection · enter/esc to return"))
	} else {
		b.WriteString("\n" + dimStyle.Render("  ctrl+c to cancel"))
	}
	return b.String()
}

func bar(pct, w int) string {
	if w < 4 {
		w = 4
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * w / 100
	return "[" + selectedStyle.Render(strings.Repeat("█", filled)) +
		dimStyle.Render(strings.Repeat("░", w-filled)) + fmt.Sprintf("] %3d%%", pct)
}

func (m Model) footer() string {
	switch m.mode {
	case modeSearch:
		return promptStyle.Render("/") + m.input.View()
	case modeCommand:
		return promptStyle.Render(":") + m.input.View()
	}
	if m.errMsg != "" {
		return errStyle.Render(trunc(m.errMsg, m.w))
	}
	// The transport line outranks the status message: while something is
	// playing, the playhead is the most useful thing this row can carry.
	if line := m.transportLine(); line != "" {
		return trunc(line, m.w)
	}
	if m.status != "" {
		return trunc(m.status, m.w)
	}
	// The selection, destination and transfer panes carry their own key hints;
	// showing the list hint underneath them too just duplicates a line.
	switch m.mode {
	case modeSelection, modeDest, modeTransfer:
		return ""
	}
	hint := "j/k move · tab select · V range · * select-match · / search · d dest · p pull · s review · ? help · q quit"
	if m.visual {
		hint = visualStyle.Render(" VISUAL ") + " j/k extend · tab toggle range · esc cancel"
	}
	return dimStyle.Render(trunc(hint, m.w))
}

// transportLine is the audio playhead: what is loaded, where it is, and at
// what speed. Empty when nothing is loaded, so it costs no screen row in the
// 99% of the app that isn't the Audio tab.
func (m Model) transportLine() string {
	if m.playErr != "" {
		return errStyle.Render("playback: " + m.playErr)
	}
	if m.playLoad != "" {
		return dimStyle.Render("⏳ " + trunc(m.playLoad, max(10, m.w-20)))
	}
	_, name := m.player.Loaded()
	if name == "" {
		return ""
	}
	icon := "⏸"
	if m.player.Playing() {
		icon = "▶"
	}
	pos := preview.FormatPosition(m.player.Position(), m.player.Duration())
	speed := ""
	if s := m.player.Speed(); s != 1 {
		speed = fmt.Sprintf(" · %.2fx", s)
	}
	tail := fmt.Sprintf(" %s%s  %s", pos, speed,
		dimStyle.Render("space play/pause · h/l ±5s · H/L ±30s · [/] speed"))
	nameW := max(10, m.w-lipgloss.Width(tail)-4)
	return selectedStyle.Render(icon) + " " + trunc(name, nameW) + tail
}

func (m Model) helpView() string {
	rows := [][2]string{
		{"j / k", "down / up"},
		{"gg / G", "top / bottom"},
		{"ctrl+d / ctrl+u", "half page down / up"},
		{"1–5, gt / gT", "switch tab"},
		{"tab / space", "toggle selection"},
		{"v / V", "anchor range at cursor (j/k extends, any select key commits)"},
		{"ctrl+a", "select all visible"},
		{"ctrl+x", "clear all visible from selection"},
		{"*", "select everything matching the current filter"},
		{"y", "add to selection without toggling"},
		{"space (Audio tab)", "play / pause the file under the cursor"},
		{"h / l (Audio tab)", "seek ∓5s"},
		{"H / L (Audio tab)", "seek ∓30s"},
		{"[ / ] (Audio tab)", "playback speed down / up (0.5x–2x)"},
		{"s", "selection review (d removes, c clears)"},
		{"d", "destination picker"},
		{"p", "pull selection to destination"},
		{"/", "fuzzy search (esc keeps filter, ctrl+c clears)"},
		{"r", "re-index device"},
		{":", "command mode"},
		{"q", "quit"},
	}
	cmds := [][2]string{
		{":dest <path>", "set destination"},
		{":mkdir <name>", "create directory"},
		{":sort date|name|size", "change ordering"},
		{":policy skip|overwrite|rename", "collision handling"},
		{":filter <query>", "set filter"},
		{":bucket <name>", "filter to one album/folder (:bucket clear resets)"},
		{":buckets", "browse every album/folder in this tab, with file counts"},
		{":clear", "clear selection"},
		{":refresh", "re-index"},
		{":pull", "start transfer"},
		{":set preview on|off", "toggle image preview on cursor rest"},
		{":set autoplay on|off", "video frame grab on cursor rest (default off)"},
		{":set audio on|off", "audio auto-play on j/k in the Audio tab"},
		{":q / :q! / :wq", "quit / discard / save"},
		{"tab (in :)", "complete the command or its argument"},
		{"up / down (in :)", "walk this session's command history"},
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render(" placer — keys ") + "\n\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %s %s\n", promptStyle.Render(pad(r[0], 18)), r[1]))
	}
	b.WriteString("\n" + titleStyle.Render(" commands ") + "\n\n")
	for _, r := range cmds {
		b.WriteString(fmt.Sprintf("  %s %s\n", promptStyle.Render(pad(r[0], 30)), r[1]))
	}
	b.WriteString("\n" + dimStyle.Render("  any key to return"))
	return b.String()
}
