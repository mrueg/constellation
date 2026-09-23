package gh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ghCLITimeout bounds the wait for `gh auth token`. A gh that hangs — a stuck
// credential helper, an unreachable keyring — would otherwise hold the whole
// program with nothing to show for it.
const ghCLITimeout = 5 * time.Second

// ResolveToken finds the GitHub token to use, once: the explicit value if
// there is one, then $GITHUB_TOKEN, then $GH_TOKEN, and finally whatever the
// gh CLI has stored. When every source comes up empty the error names all of
// them, so the reader knows which one to fill in.
//
// A command should call this once at its start and hand the result to every
// client it builds. The constructors deliberately do no discovery of their
// own: each one that did repeated the gh fallback, so a run needing three
// clients could spend three timeouts on a stuck keyring and report the second
// failure as something other than a missing token.
func ResolveToken(ctx context.Context, explicit string) (string, error) {
	if t := strings.TrimSpace(explicit); t != "" {
		return t, nil
	}
	if t := firstEnv("GITHUB_TOKEN", "GH_TOKEN"); t != "" {
		return t, nil
	}
	t, err := ghCLIToken(ctx)
	if err != nil {
		return "", fmt.Errorf("no GitHub token: --token was not given, $GITHUB_TOKEN and $GH_TOKEN are unset, "+
			"and `gh auth token` gave none (%w); set one of them or run `gh auth login`", err)
	}
	return t, nil
}

// ghCLIToken asks the gh CLI for its stored token. The argv is fixed, so
// nothing the user typed reaches a shell, and the deadline keeps a wedged gh
// from hanging the program — an interrupt cuts it short as well, because the
// context handed in is the one the signal handler cancels.
func ghCLIToken(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// gh explains itself on stderr ("no oauth token found"), which is
			// more useful than the exit status alone.
			if msg := firstLine(exit.Stderr); msg != "" {
				return "", errors.New(msg)
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("%w after %s", ctxErr, ghCLITimeout)
		}
		return "", err
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", errors.New("gh printed an empty token")
	}
	return t, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}
