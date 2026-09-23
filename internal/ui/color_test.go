package ui

import (
	"os"
	"strings"
	"testing"
)

// Colour must be opt-out in the ways users expect, and must never appear in
// redirected output — a plan piped to a file or a log captured by CI has to
// stay free of escape sequences.
func TestDisabledProducesPlainText(t *testing.T) {
	was := Enabled()
	t.Cleanup(func() { SetEnabled(was) })
	SetEnabled(false)
	for name, got := range map[string]string{
		"Title":   Title("x"),
		"Added":   Added("x"),
		"Removed": Removed("x"),
		"Warn":    Warn("x"),
		"Muted":   Muted("x"),
		"Name":    Name("x"),
		"Info":    Info("x"),
	} {
		if got != "x" {
			t.Errorf("%s returned %q with colour off, want plain", name, got)
		}
	}
}

func TestEnabledWrapsAndResets(t *testing.T) {
	was := Enabled()
	t.Cleanup(func() { SetEnabled(was) })
	SetEnabled(true)
	got := Added("done")
	if !strings.Contains(got, "done") {
		t.Fatalf("text was lost: %q", got)
	}
	if got == "done" {
		t.Errorf("colour was not applied: %q", got)
	}
	if !strings.HasSuffix(got, "\033[0m") {
		t.Errorf("%q does not reset, so colour would bleed into later output", got)
	}
}

// An empty string must stay empty: wrapping it would emit a bare escape pair
// that shifts column alignment for no visible text.
func TestEmptyStringIsNotWrapped(t *testing.T) {
	was := Enabled()
	t.Cleanup(func() { SetEnabled(was) })
	SetEnabled(true)
	if got := Title(""); got != "" {
		t.Errorf("empty string became %q", got)
	}
}

// Padding measures display width, not runes. CJK characters and emoji occupy
// two columns each, so counting runes left those rows short and the lists table
// ragged — which is what a user sees, since GitHub list names often carry
// emoji.
func TestPadMeasuresDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
	}{
		{"Kubernetes", 10},
		{"日本語ツール", 12}, // six runes, twelve columns
		{"Go 🚀", 5},    // emoji is two columns wide
		{"", 0},
	} {
		if got := Width(tc.in); got != tc.width {
			t.Errorf("Width(%q) = %d, want %d", tc.in, got, tc.width)
		}
		// Padding plus content must land exactly on the column.
		if total := Width(tc.in) + len(Pad(tc.in, 34)); total != 34 {
			t.Errorf("%q padded to %d columns, want 34", tc.in, total)
		}
	}
}

// A string at or past the target width still gets a separating space, so two
// columns never run together.
func TestPadNeverReturnsEmpty(t *testing.T) {
	if got := Pad(strings.Repeat("x", 40), 34); got != " " {
		t.Errorf("Pad on an over-long string returned %q, want a single space", got)
	}
}

// Every variable that can influence Configure, so a test can start from a
// known environment regardless of the shell that ran `go test`.
var colorEnv = []string{"NO_COLOR", "CLICOLOR_FORCE", "CLICOLOR", "TERM"}

// unsetenv removes keys for the test's duration. t.Setenv cannot express
// "absent", and for NO_COLOR an empty value is not absent.
func unsetenv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, v) })
			_ = os.Unsetenv(k)
		}
	}
}

// configureWith runs Configure under exactly the given variables and restores
// the package's state afterwards.
func configureWith(t *testing.T, env map[string]string) {
	t.Helper()
	was := Enabled()
	t.Cleanup(func() { SetEnabled(was) })
	unsetenv(t, colorEnv...)
	for k, v := range env {
		t.Setenv(k, v)
	}
	Configure()
}

// fatih/color captures NO_COLOR into each Color when it is built, which pinned
// colour off for the life of the process and made SetEnabled(true) a no-op
// whenever the shell had NO_COLOR exported — `NO_COLOR=1 go test` failed.
func TestSetEnabledWinsOverNoColor(t *testing.T) {
	configureWith(t, map[string]string{"NO_COLOR": "1", "CLICOLOR_FORCE": "1"})
	if Enabled() {
		t.Fatal("NO_COLOR did not disable colour")
	}
	SetEnabled(true)
	if !Enabled() {
		t.Fatal("SetEnabled(true) did not report enabled")
	}
	if got := Added("x"); got == "x" || !strings.HasSuffix(got, "\033[0m") {
		t.Errorf("SetEnabled(true) under NO_COLOR produced %q, want colour", got)
	}
	SetEnabled(false)
	if got := Added("x"); got != "x" {
		t.Errorf("SetEnabled(false) produced %q, want plain", got)
	}
}

// no-color.org: the variable counts when set, regardless of its value, so an
// empty NO_COLOR still switches colour off — and it outranks CLICOLOR_FORCE.
func TestNoColorEmptyValueDisables(t *testing.T) {
	configureWith(t, map[string]string{"NO_COLOR": "", "CLICOLOR_FORCE": "1"})
	if Enabled() {
		t.Error("an empty NO_COLOR did not disable colour")
	}
}

// CLICOLOR_FORCE forces colour even when output is not a terminal, which is
// the case under `go test`.
func TestCLICOLORForceEnablesWithoutTerminal(t *testing.T) {
	configureWith(t, map[string]string{"CLICOLOR_FORCE": "1", "TERM": "dumb", "CLICOLOR": "0"})
	if !Enabled() {
		t.Error("CLICOLOR_FORCE=1 did not force colour on")
	}
}

// Per bixense.com/clicolors, CLICOLOR_FORCE=0 means "not forced": the result
// must be whatever it would have been with the variable absent, never "on".
func TestCLICOLORForceZeroDoesNotForce(t *testing.T) {
	configureWith(t, nil)
	base := Enabled()
	configureWith(t, map[string]string{"CLICOLOR_FORCE": "0"})
	if got := Enabled(); got != base {
		t.Errorf("CLICOLOR_FORCE=0 changed the decision from %v to %v", base, got)
	}
	configureWith(t, map[string]string{"CLICOLOR_FORCE": "0", "CLICOLOR": "0"})
	if Enabled() {
		t.Error("CLICOLOR_FORCE=0 overrode CLICOLOR=0")
	}
}

func TestCLICOLORZeroDisables(t *testing.T) {
	configureWith(t, map[string]string{"CLICOLOR": "0", "TERM": "xterm-256color"})
	if Enabled() {
		t.Error("CLICOLOR=0 did not disable colour")
	}
}

func TestTermDumbDisables(t *testing.T) {
	for _, term := range []string{"dumb", "DUMB"} {
		configureWith(t, map[string]string{"TERM": term})
		if Enabled() {
			t.Errorf("TERM=%s did not disable colour", term)
		}
	}
}
