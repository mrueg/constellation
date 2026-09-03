// Package ui applies colour and alignment to terminal output.
package ui

import (
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/mattn/go-runewidth"
)

// Configure applies the environment's colour preferences. It is called once
// from main, rather than from an init function, so that the decision happens at
// a point the program controls: package initialisation order is implicit, runs
// even in tests that never wanted it, and hides a global mutation in a place
// nothing calls.
//
// Colour is off unless stdout is a terminal, so a redirected plan or a piped
// log stays free of escape sequences. fatih/color decides that part — it checks
// the terminal and honours NO_COLOR (https://no-color.org) — and the rules here
// are the ones it does not cover: CLICOLOR_FORCE for keeping colour through a
// pager, and TERM=dumb.
func Configure() {
	switch {
	case os.Getenv("NO_COLOR") != "":
		color.NoColor = true
	case os.Getenv("CLICOLOR_FORCE") != "":
		color.NoColor = false
	case strings.EqualFold(os.Getenv("TERM"), "dumb"):
		color.NoColor = true
	}
}

// SetEnabled overrides the detection, for tests and for a future --color flag.
func SetEnabled(v bool) { color.NoColor = !v }

// Enabled reports whether colour is being applied.
func Enabled() bool { return !color.NoColor }

// The names describe the role rather than the colour, so the palette can change
// in one place without every call site becoming a lie.
var (
	title   = color.New(color.Bold).SprintFunc()
	added   = color.New(color.FgGreen).SprintFunc()
	removed = color.New(color.FgRed).SprintFunc()
	warn    = color.New(color.FgYellow).SprintFunc()
	muted   = color.New(color.Faint).SprintFunc()
	name    = color.New(color.FgCyan).SprintFunc()
	info    = color.New(color.FgBlue).SprintFunc()
)

// Wrapping an empty string would emit a bare escape pair: no visible text, but
// it still shifts anything that measures the result.
func apply(f func(...any) string, s string) string {
	if s == "" {
		return s
	}
	return f(s)
}

// Title is a heading: a category name, a section.
func Title(s string) string { return apply(title, s) }

// Added marks something created or filed.
func Added(s string) string { return apply(added, s) }

// Removed marks something deleted or taken away.
func Removed(s string) string { return apply(removed, s) }

// Warn marks something the user should look at but which is not an error.
func Warn(s string) string { return apply(warn, s) }

// Muted is secondary detail: counts, terms, timings.
func Muted(s string) string { return apply(muted, s) }

// Name marks an identifier — a list or repository name.
func Name(s string) string { return apply(name, s) }

// Info marks a neutral highlight, such as a status word.
func Info(s string) string { return apply(info, s) }

// Pad returns the spaces needed to bring s up to width columns.
//
// It measures display width, not runes: a star list named with CJK text or an
// emoji occupies two columns per character, so counting runes left those rows
// short and the table ragged. Colour is applied around the padding rather than
// inside it, since escape sequences occupy no columns but would still be
// counted by a fixed-width verb.
func Pad(s string, width int) string {
	if n := width - runewidth.StringWidth(s); n > 0 {
		return strings.Repeat(" ", n)
	}
	return " "
}

// Width reports how many terminal columns s occupies.
func Width(s string) int { return runewidth.StringWidth(s) }
