package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/CExSDixit/placer/internal/device"
	"github.com/CExSDixit/placer/internal/index"
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
	}
	return m.header() + "\n" + m.listView() + "\n" + m.footer()
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
	size, date, kind = 8, 16, 7
	name = m.w - size - date - kind - 8
	if name > maxNameCol {
		name = maxNameCol
	}
	if name < 12 {
		name = 12
	}
	return
}

func (m Model) listView() string {
	if m.loading {
		return "\n  " + dimStyle.Render("indexing device…")
	}
	if len(m.view.Files) == 0 {
		if m.query != "" {
			return "\n  " + dimStyle.Render("no matches for "+m.query)
		}
		return "\n  " + dimStyle.Render("no files in this tab")
	}

	nameW, sizeW, dateW, kindW := m.columns()
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
	if m.status != "" {
		return trunc(m.status, m.w)
	}
	// The selection, destination and transfer panes carry their own key hints;
	// showing the list hint underneath them too just duplicates a line.
	switch m.mode {
	case modeSelection, modeDest, modeTransfer:
		return ""
	}
	hint := "j/k move · tab select · v visual · / search · d dest · p pull · s review · ? help · q quit"
	if m.visual {
		hint = visualStyle.Render(" VISUAL ") + " j/k extend · tab toggle range · esc cancel"
	}
	return dimStyle.Render(trunc(hint, m.w))
}

func (m Model) helpView() string {
	rows := [][2]string{
		{"j / k", "down / up"},
		{"gg / G", "top / bottom"},
		{"ctrl+d / ctrl+u", "half page down / up"},
		{"1–5, gt / gT", "switch tab"},
		{"tab / space", "toggle selection"},
		{"v", "visual mode (j/k extends, tab toggles range)"},
		{"V", "select all visible"},
		{"y", "add to selection without toggling"},
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
		{":clear", "clear selection"},
		{":refresh", "re-index"},
		{":pull", "start transfer"},
		{":q / :q! / :wq", "quit / discard / save"},
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
