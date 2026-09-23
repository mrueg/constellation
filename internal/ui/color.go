// Package ui applies colour and alignment to terminal output.
package ui

import (
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/mattn/go-isatty"
	"github.com/mattn/go-runewidth"
)

// enabled is the single source of truth for whether colour is applied. It is
// pushed into every Color in the palette by SetEnabled, so the palette never
// consults fatih/color's own detection.
//
// Until Configure runs it mirrors fatih/color's default (stdout is a terminal,
// NO_COLOR unset, TERM not dumb), so output looks the same whether or not the
// caller has configured the package yet.
var enabled = !color.NoColor

// Configure applies the environment's colour preferences. It is called once
// from main, rather than from an init function, so that the decision happens at
// a point the program controls: package initialisation order is implicit, runs
// even in tests that never wanted it, and hides a global mutation in a place
// nothing calls.
//
// The decision is made here rather than left to fatih/color, for two reasons.
// fatih/color checks only stdout, but main writes warnings and errors to
// stderr, so `constellation plan 2>err.log` would fill the log with escape
// sequences. And fatih/color captures NO_COLOR into each Color at construction,
// which pins colour off for the life of the process and stops SetEnabled from
// turning it back on; keeping the flag here lets SetEnabled always win.
//
// The environment conventions honoured, in order of precedence:
//
//   - NO_COLOR set, whatever its value (https://no-color.org): off.
//   - CLICOLOR_FORCE set to anything but "0" (https://bixense.com/clicolors):
//     on, even when output is not a terminal — the way to keep colour through
//     a pager or a CI log that renders it.
//   - CLICOLOR=0: off.
//   - TERM=dumb: off.
//   - Otherwise on only when both stdout and stderr are terminals.
func Configure() { SetEnabled(detect()) }

func detect() bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		return true
	}
	if os.Getenv("CLICOLOR") == "0" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	// Both streams must be terminals. The palette is used on stdout and stderr
	// alike, and a user who redirects either one expects a file free of escape
	// sequences; checking stdout alone would colour stderr into a captured
	// error log, and checking stderr alone would colour a piped plan.
	return isTerminal(os.Stdout) && isTerminal(os.Stderr)
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// SetEnabled overrides the detection, for tests and for a future --color flag.
// It always wins: each Color is switched explicitly, so neither fatih/color's
// global NoColor nor the NO_COLOR it captured at construction can veto it.
func SetEnabled(v bool) {
	enabled = v
	for _, c := range palette {
		if v {
			c.EnableColor()
		} else {
			c.DisableColor()
		}
	}
}

// Enabled reports whether colour is being applied.
func Enabled() bool { return enabled }

// The names describe the role rather than the colour, so the palette can change
// in one place without every call site becoming a lie.
var (
	title   = newColor(color.Bold)
	added   = newColor(color.FgGreen)
	removed = newColor(color.FgRed)
	warn    = newColor(color.FgYellow)
	muted   = newColor(color.Faint)
	name    = newColor(color.FgCyan)
	info    = newColor(color.FgBlue)

	palette = []*color.Color{title, added, removed, warn, muted, name, info}
)

// newColor builds a Color that follows the package's enabled flag from the
// start, rather than the NO_COLOR value fatih/color would otherwise pin into it.
func newColor(attrs ...color.Attribute) *color.Color {
	c := color.New(attrs...)
	if enabled {
		c.EnableColor()
	} else {
		c.DisableColor()
	}
	return c
}

// Wrapping an empty string would emit a bare escape pair: no visible text, but
// it still shifts anything that measures the result.
func apply(c *color.Color, s string) string {
	if s == "" {
		return s
	}
	return c.Sprint(s)
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
