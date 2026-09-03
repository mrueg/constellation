package ui

import (
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
