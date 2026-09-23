package gh

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// noEnvToken clears both environment sources for the test, so what it sees
// is the precedence under test and not the developer's own credentials.
func noEnvToken(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
}

// fakeGh puts a shell script named gh first on PATH. Every invocation appends
// its arguments to a log file, whose path is returned, and then runs body —
// the script's stdout is the "stored token".
func fakeGh(t *testing.T, body string) (log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + body + "\n"
	path := filepath.Join(dir, "gh")
	// The script has to be executable to stand in for gh; the directory is
	// private to the test.
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // an executable stub
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return log
}

func ghCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// The flag beats the environment, GITHUB_TOKEN beats GH_TOKEN, and none of
// those cases should go anywhere near the gh CLI.
func TestResolveTokenPrecedence(t *testing.T) {
	log := fakeGh(t, "echo from-gh")
	for _, tc := range []struct {
		name, explicit, github, gh, want string
	}{
		{name: "explicit flag wins", explicit: "from-flag", github: "from-github", gh: "from-gh-env", want: "from-flag"},
		{name: "GITHUB_TOKEN beats GH_TOKEN", github: "from-github", gh: "from-gh-env", want: "from-github"},
		{name: "GH_TOKEN alone", gh: "from-gh-env", want: "from-gh-env"},
		{name: "whitespace is not a token", explicit: "  ", github: " \n", gh: "from-gh-env", want: "from-gh-env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tc.github)
			t.Setenv("GH_TOKEN", tc.gh)
			got, err := ResolveToken(context.Background(), tc.explicit)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ResolveToken = %q, want %q", got, tc.want)
			}
		})
	}
	if calls := ghCalls(t, log); len(calls) != 0 {
		t.Errorf("gh was run %d times although a token was set: %q", len(calls), calls)
	}
}

// With nothing set, the gh CLI is the source — and it is asked exactly once
// per resolve, with a fixed argv. Before this each client constructor asked on
// its own, so one command could pay the subprocess's deadline three times.
func TestResolveTokenAsksGhOnce(t *testing.T) {
	noEnvToken(t)
	log := fakeGh(t, "echo '  ghp_stored  '")
	got, err := ResolveToken(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ghp_stored" {
		t.Errorf("ResolveToken = %q, want the trimmed token gh printed", got)
	}
	calls := ghCalls(t, log)
	if len(calls) != 1 {
		t.Fatalf("gh was run %d times for one resolve, want exactly 1: %q", len(calls), calls)
	}
	if calls[0] != "auth token" {
		t.Errorf("gh was run with %q, want \"auth token\"", calls[0])
	}

	// Building both clients from the result must not ask again: the whole
	// point of resolving up front.
	if _, err := NewClient(got); err != nil {
		t.Fatal(err)
	}
	if _, err := NewListsClient(got); err != nil {
		t.Fatal(err)
	}
	if calls := ghCalls(t, log); len(calls) != 1 {
		t.Errorf("building clients ran gh again: %d runs in total", len(calls))
	}
}

// When no source has a token, the error names every one that was tried and
// carries gh's own reason, so the reader knows what to set rather than only
// that something is missing.
func TestResolveTokenErrorNamesEverySource(t *testing.T) {
	noEnvToken(t)
	fakeGh(t, "echo 'no oauth token found' >&2; exit 1")
	_, err := ResolveToken(context.Background(), "")
	if err == nil {
		t.Fatal("ResolveToken found a token where there was none")
	}
	for _, want := range []string{"--token", "GITHUB_TOKEN", "GH_TOKEN", "gh auth token", "no oauth token found", "gh auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	// A gh that prints nothing is a missing token too, not an empty one.
	fakeGh(t, "true")
	if _, err := ResolveToken(context.Background(), ""); err == nil {
		t.Error("an empty line from gh was accepted as a token")
	}

	// And a gh that is not installed at all.
	t.Setenv("PATH", t.TempDir())
	if _, err := ResolveToken(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("without gh on PATH: %v, want the sources named", err)
	}
}

// The constructors take a resolved token and do no discovery of their own;
// an empty token is a programming error, not a cue to go looking.
func TestClientsDoNotDiscoverATokenThemselves(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "from-env")
	t.Setenv("GH_TOKEN", "from-env")
	log := fakeGh(t, "echo from-gh")
	if c, err := NewClient(""); err == nil {
		t.Errorf("NewClient(\"\") = %v, want an error", c)
	}
	if c, err := NewListsClient(""); err == nil {
		t.Errorf("NewListsClient(\"\") = %v, want an error", c)
	}
	if calls := ghCalls(t, log); len(calls) != 0 {
		t.Errorf("a constructor ran gh: %q", calls)
	}
}
