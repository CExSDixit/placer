package ui

import (
	"sort"
	"strings"
)

// Command-mode quality of life: tab completion and shell-style history.
//
// Both are deliberately session-only. `:` commands here are neither expensive
// nor sensitive enough to be worth a file on disk, and a history that
// survives restarts would mostly replay the last session's destination paths
// at someone who has since moved on.

// commandNames is every command runCommand accepts, in the order they are
// offered. Keep in sync with the switch in runCommand and the help screen.
var commandNames = []string{
	"bucket", "buckets", "clear", "dest", "filter", "mkdir", "policy",
	"pull", "q", "q!", "refresh", "set", "sort", "wq",
}

// staticArgs are the commands whose arguments are a fixed, short list.
var staticArgs = map[string][]string{
	"sort":   {"date", "name", "size"},
	"policy": {"skip", "overwrite", "rename"},
	"set":    {"audio", "autoplay", "preview"},
}

// setValues completes the second word of `:set <toggle> …`.
var setValues = []string{"off", "on"}

// candidates returns the completions available for a partially typed command
// line, along with the prefix they should replace.
func (m Model) candidates(line string) (prefix string, opts []string) {
	// Leading spaces are not meaningful; a trailing one means "the next word
	// is empty", which is what makes `:set <tab>` offer every toggle.
	trimmed := strings.TrimLeft(line, " ")
	fields := strings.Fields(trimmed)
	atWordStart := trimmed == "" || strings.HasSuffix(trimmed, " ")

	switch {
	case len(fields) == 0 || (len(fields) == 1 && !atWordStart):
		word := ""
		if len(fields) == 1 {
			word = fields[0]
		}
		return word, matching(commandNames, word)

	case fields[0] == "bucket":
		// Bucket names come from the live index, so this completes against
		// the albums that actually exist in the current tab.
		var names []string
		if m.ix != nil {
			for _, b := range m.ix.Buckets(m.tab) {
				if b.Name != "(none)" {
					names = append(names, b.Name)
				}
			}
		}
		names = append(names, "clear")
		sort.Strings(names)
		return lastWord(fields, atWordStart), matching(names, lastWord(fields, atWordStart))

	case fields[0] == "set" && (len(fields) > 2 || (len(fields) == 2 && atWordStart)):
		word := lastWord(fields, atWordStart)
		return word, matching(setValues, word)

	default:
		if opts, ok := staticArgs[fields[0]]; ok && len(fields) <= 2 {
			word := lastWord(fields, atWordStart)
			return word, matching(opts, word)
		}
	}
	return "", nil
}

func lastWord(fields []string, atWordStart bool) string {
	if atWordStart || len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func matching(all []string, prefix string) []string {
	var out []string
	for _, c := range all {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// completeCommand returns the line after a tab press, plus a hint to show
// when the completion was ambiguous.
//
// One match completes and adds a trailing space, so `:se<tab>` lands on
// `:set ` ready for its argument. Several matches extend as far as they
// unambiguously agree and list the rest — the shell behaviour the muscle
// memory expects.
func (m Model) completeCommand(line string) (string, string) {
	prefix, opts := m.candidates(line)
	if len(opts) == 0 {
		return line, ""
	}
	head := line[:len(line)-len(prefix)]
	if len(opts) == 1 {
		return head + opts[0] + " ", ""
	}
	common := longestCommonPrefix(opts)
	if len(common) > len(prefix) {
		return head + common, strings.Join(opts, "  ")
	}
	return line, strings.Join(opts, "  ")
}

func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

// pushHistory records a command line, most recent last, skipping blanks and
// immediate repeats.
func (m *Model) pushHistory(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if n := len(m.cmdHistory); n > 0 && m.cmdHistory[n-1] == line {
		return
	}
	m.cmdHistory = append(m.cmdHistory, line)
	if len(m.cmdHistory) > maxCmdHistory {
		m.cmdHistory = m.cmdHistory[len(m.cmdHistory)-maxCmdHistory:]
	}
}

// historyStep walks the history: -1 is back (up arrow), +1 forward. Index
// len(history) means "the line being typed", which is stashed on the first
// step back so arrowing all the way forward restores it.
func (m *Model) historyStep(dir int) {
	if len(m.cmdHistory) == 0 {
		return
	}
	if m.cmdHistIdx == len(m.cmdHistory) {
		m.cmdDraft = m.input.Value()
	}
	i := m.cmdHistIdx + dir
	if i < 0 {
		i = 0
	}
	if i > len(m.cmdHistory) {
		i = len(m.cmdHistory)
	}
	m.cmdHistIdx = i
	if i == len(m.cmdHistory) {
		m.input.SetValue(m.cmdDraft)
	} else {
		m.input.SetValue(m.cmdHistory[i])
	}
	m.input.CursorEnd()
}
