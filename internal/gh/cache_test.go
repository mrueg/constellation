package gh

import (
	"encoding/json"
	"fmt"
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
	got, err := LoadCache(path, "octocat", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || got.Repos[0].FullName != "a/one" {
		t.Fatalf("stars did not survive the round trip: %+v", got.Repos)
	}
}

// A full walk is the only read that revisits every repository, so it is the
// only one that may set the full-fetch time. SaveCache records both stamps.
func TestSaveCacheStampsAFullFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stars.json")
	before := time.Now()
	if err := SaveCache(path, "octocat", []Repo{{ID: 1, FullName: "a/one"}}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCache(path, "octocat", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.FetchedAt.Before(before) || got.FullFetchedAt.Before(before) {
		t.Errorf("a full save stamped fetched %v, full %v; both should be no earlier than %v", got.FetchedAt, got.FullFetchedAt, before)
	}
	if !got.FullFetchedAt.Equal(got.FetchedAt) {
		t.Errorf("a full save stamped fetched %v but full %v; they should agree", got.FetchedAt, got.FullFetchedAt)
	}
}

// A top-up reads only the tail of the list, so it advances the cache's age
// for --cache-ttl but must leave the full-fetch time where the last full walk
// put it. Stamping both on every save is what kept a cache topped up daily
// from ever being re-read in full.
func TestTopUpCacheKeepsTheFullFetchTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stars.json")
	fullAt := time.Now().Add(-20 * 24 * time.Hour).Truncate(time.Second)
	writtenAt := fullAt.Add(24 * time.Hour)
	b, err := json.Marshal(cacheFile{FetchedAt: writtenAt, FullFetchedAt: fullAt, User: "octocat", Repos: []Repo{{ID: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	stale, err := LoadCache(path, "octocat", 0)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := TopUpCache(path, "octocat", []Repo{{ID: 1}, {ID: 2}}, stale.FullFetchedAt); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCache(path, "octocat", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 2 {
		t.Errorf("the topped-up list has %d stars, want 2", len(got.Repos))
	}
	if got.FetchedAt.Before(before) {
		t.Errorf("a top-up left fetched_at at %v; it should have advanced to at least %v", got.FetchedAt, before)
	}
	if !got.FullFetchedAt.Equal(fullAt) {
		t.Errorf("a top-up moved full_fetched_at from %v to %v", fullAt, got.FullFetchedAt)
	}
}

// A cache written before the full-fetch time existed carries only fetched_at.
// When it was last walked in full cannot be known, and reading the missing
// stamp as "never" would force everyone into a full re-read on upgrade, so it
// is taken to be the write time; the next full walk records it for real.
func TestLoadCacheTreatsALegacyFileAsFullyFetchedWhenWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stars.json")
	fetched := time.Now().Add(-3 * 24 * time.Hour).Truncate(time.Second)
	legacy := fmt.Sprintf(`{"fetched_at":%q,"user":"octocat","repos":[{"id":1,"full_name":"a/one"}]}`, fetched.Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCache(path, "octocat", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("loaded %d stars, want 1", len(got.Repos))
	}
	if !got.FetchedAt.Equal(fetched) {
		t.Errorf("fetched_at = %v, want %v", got.FetchedAt, fetched)
	}
	if !got.FullFetchedAt.Equal(fetched) {
		t.Errorf("a legacy file loaded with full_fetched_at %v, want fetched_at %v", got.FullFetchedAt, fetched)
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
