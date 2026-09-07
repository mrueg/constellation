package gh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadmeCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readmes.json")
	want := ReadmeCache{
		"a/one": {Text: "a small tool", FetchedAt: time.Now()},
		"b/two": {Text: " ", FetchedAt: time.Now()}, // the "unreadable" marker
	}
	if err := SaveReadmes(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadReadmes(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, e := range want {
		if got[name].Text != e.Text {
			t.Errorf("%s = %q, want %q", name, got[name].Text, e.Text)
		}
	}
	if len(got) != len(want) {
		t.Errorf("loaded %d entries, want %d", len(got), len(want))
	}
}

// Entries expire one by one, since a run only re-reads what is missing: a
// single timestamp for the file would mean either everything expires together
// or, once untouched entries were re-stamped, nothing ever expires.
func TestReadmeCacheExpiresPerEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readmes.json")
	if err := SaveReadmes(path, ReadmeCache{
		"old/one": {Text: "stale", FetchedAt: time.Now().Add(-48 * time.Hour)},
		"new/two": {Text: "fresh", FetchedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LoadReadmes(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["old/one"]; ok {
		t.Error("an entry past the TTL was kept")
	}
	if _, ok := got["new/two"]; !ok {
		t.Error("an entry inside the TTL was dropped")
	}

	if got, err = LoadReadmes(path, 0); err != nil {
		t.Fatal(err)
	} else if len(got) != 2 {
		t.Errorf("a zero TTL kept %d of 2 entries; it should keep every one", len(got))
	}
}

// A cache that cannot be used costs a re-fetch, which is not a failure worth
// stopping a run for.
func TestLoadReadmesTreatsAnUnusableCacheAsEmpty(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.json")
	got, err := LoadReadmes(missing, 0)
	if err != nil || len(got) != 0 {
		t.Errorf("missing cache = (%v, %v), want an empty cache and no error", got, err)
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err = LoadReadmes(corrupt, 0); err != nil || len(got) != 0 {
		t.Errorf("corrupt cache = (%v, %v), want an empty cache and no error", got, err)
	}

	wrongVersion := filepath.Join(dir, "future.json")
	if err := os.WriteFile(wrongVersion, []byte(`{"version":99,"entries":{"a/one":{"text":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err = LoadReadmes(wrongVersion, 0); err != nil || len(got) != 0 {
		t.Errorf("future cache = (%v, %v), want an empty cache and no error", got, err)
	}
}

// The star cache must not carry README text any more: it is the file that
// expires in a day, and duplicating a megabyte of prose into it is what tied
// the READMEs to that expiry.
func TestSaveCacheDropsReadmeText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stars.json")
	repos := []Repo{{ID: 1, FullName: "a/one", Readme: "a distinctive readme opening"}}
	if err := SaveCache(path, "octocat", repos); err != nil {
		t.Fatal(err)
	}
	if repos[0].Readme == "" {
		t.Error("SaveCache emptied the caller's copy instead of only the stored one")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "distinctive readme opening") {
		t.Error("the star cache still holds README text")
	}
	got, _, err := LoadCache(path, "octocat", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FullName != "a/one" {
		t.Fatalf("stars did not survive the round trip: %+v", got)
	}
}

// Upgrading from a version that kept READMEs inside the star cache must not
// cost one API call per star. The star cache's own age and owner say nothing
// about whether a README is still good, so neither is consulted.
func TestReadmesFromStarCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stars.json")
	fetched := time.Now().Add(-72 * time.Hour)
	b, err := json.Marshal(cacheFile{
		FetchedAt: fetched,
		User:      "someone-else",
		Repos: []Repo{
			{FullName: "a/one", Readme: "an opening"},
			{FullName: "b/two"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	got := ReadmesFromStarCache(path)
	if len(got) != 1 {
		t.Fatalf("harvested %d READMEs, want 1", len(got))
	}
	if got["a/one"].Text != "an opening" {
		t.Errorf("a/one = %q, want %q", got["a/one"].Text, "an opening")
	}
	if !got["a/one"].FetchedAt.Equal(fetched) {
		t.Errorf("entry stamped %v, want the star cache's own %v", got["a/one"].FetchedAt, fetched)
	}
	if n := len(ReadmesFromStarCache(filepath.Join(t.TempDir(), "absent.json"))); n != 0 {
		t.Errorf("harvesting a missing star cache returned %d entries", n)
	}
}
