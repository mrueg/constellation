package gh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// cacheFile is the on-disk shape of the star cache.
type cacheFile struct {
	// FetchedAt is when the file was last written, by a full walk or a top-up.
	FetchedAt time.Time `json:"fetched_at"`
	// FullFetchedAt is when every page was last read. A top-up reads only the
	// tail, so this is the age --cache-full-ttl is measured against. Files
	// written before it existed lack it; see LoadCache.
	FullFetchedAt time.Time `json:"full_fetched_at,omitzero"`
	User          string    `json:"user"`
	Repos         []Repo    `json:"repos"`
}

// Cache is a loaded star cache.
type Cache struct {
	// Repos is nil on a miss.
	Repos []Repo
	// FetchedAt is when the cache was last written, whether by a full walk or
	// a top-up. --cache-ttl is measured against it.
	FetchedAt time.Time
	// FullFetchedAt is when the star list was last read in full. A top-up
	// never revisits the repositories already cached, so a description or
	// topic changed upstream reaches the model only once this passes
	// --cache-full-ttl and every page is read again.
	FullFetchedAt time.Time
}

// DefaultCachePath returns the per-user cache location.
func DefaultCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "constellation-stars.json"
	}
	return filepath.Join(dir, "constellation", "stars.json")
}

// SaveCache stores the stars of a full walk so that re-running with different
// clustering settings costs no API calls. Both timestamps are set to now. A
// top-up, which reads only the tail, goes through TopUpCache instead, so that
// a read that revisited nothing does not count as a full one.
//
// README text is stripped: it lives in its own cache with its own expiry, and
// writing it here as well would both double the file and tie a megabyte of
// prose to the star list's one-day lifetime.
func SaveCache(path, user string, repos []Repo) error {
	now := time.Now()
	return writeCache(path, cacheFile{FetchedAt: now, FullFetchedAt: now, User: user, Repos: repos})
}

// TopUpCache stores a topped-up star list. FetchedAt becomes now, while the
// full-fetch time is carried over from the cache that was topped up
// (fullFetchedAt, as LoadCache returned it), since only the pages after the
// cached tail were read. Stamping both, as SaveCache does, is what kept a
// cache topped up more often than --cache-full-ttl from ever being re-read in
// full.
func TopUpCache(path, user string, repos []Repo, fullFetchedAt time.Time) error {
	return writeCache(path, cacheFile{FetchedAt: time.Now(), FullFetchedAt: fullFetchedAt, User: user, Repos: repos})
}

func writeCache(path string, c cacheFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	bare := make([]Repo, len(c.Repos))
	copy(bare, c.Repos)
	for i := range bare {
		bare[i].Readme = ""
	}
	c.Repos = bare
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadCache returns the cached stars for the user when they are younger than
// maxAge; a maxAge of zero or less accepts the cache at any age, which is what
// an incremental refresh reads it with. A miss — no file, a corrupt one,
// somebody else's stars — is reported as a Cache with no Repos rather than an
// error; a cache past maxAge keeps its timestamps, so the caller can say how
// old it was.
//
// A file written before the full-fetch time was recorded has FullFetchedAt
// taken from FetchedAt. When that cache was last walked in full cannot be
// known, and treating it as never would force every upgrade into a full
// re-read; the next full walk records it properly.
func LoadCache(path, user string, maxAge time.Duration) (Cache, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Cache{}, nil
		}
		return Cache{}, err
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return Cache{}, nil // a corrupt cache is a miss, not a failure
	}
	if c.FullFetchedAt.IsZero() {
		c.FullFetchedAt = c.FetchedAt
	}
	out := Cache{FetchedAt: c.FetchedAt, FullFetchedAt: c.FullFetchedAt}
	if c.User != user || (maxAge > 0 && time.Since(c.FetchedAt) > maxAge) {
		return out, nil
	}
	out.Repos = c.Repos
	return out, nil
}

// The README cache is deliberately a second file rather than a field of the
// star cache. READMEs cost one API call each and thousands of them dominate a
// first run, while the star list itself is a few dozen cheap pages that go
// stale in a day. Holding both under one expiry meant that picking up a
// handful of new stars — a --refresh, or simply a cache older than
// --cache-ttl — threw away every README with them and re-fetched the lot.
// Keyed by full name, so a README also survives a repository moving in or out
// of the filtered set.

// CachedReadme is one stored README opening. The timestamp is per entry
// because entries are written at different times: a run fetches only what is
// missing, and stamping the untouched ones again would mean nothing ever
// expired.
type CachedReadme struct {
	Text      string    `json:"text"`
	FetchedAt time.Time `json:"fetched_at"`
}

// ReadmeCache maps a repository's full name to the opening of its README.
type ReadmeCache map[string]CachedReadme

// readmeFile is the on-disk shape of the README cache.
type readmeFile struct {
	Version int         `json:"version"`
	Entries ReadmeCache `json:"entries"`
}

// readmeCacheVersion guards the file against a change of shape or meaning. A
// mismatch is treated as a miss, which costs a re-fetch and no correctness.
//
// Version 1 held the first bytes of each README, stripped and collapsed.
// Version 2 holds the README's opening summary, chosen by section before
// anything is cut; the old text cannot be re-read into that shape, so the
// first run after the upgrade fetches every README again.
const readmeCacheVersion = 2

// DefaultReadmeCachePath returns the per-user README cache location.
func DefaultReadmeCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "constellation-readmes.json"
	}
	return filepath.Join(dir, "constellation", "readmes.json")
}

// LoadReadmes returns the cached README openings, dropping entries older than
// maxAge; maxAge of zero or less keeps every entry. A missing or unreadable
// cache is a miss rather than a failure, since the only cost is re-fetching.
func LoadReadmes(path string, maxAge time.Duration) (ReadmeCache, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ReadmeCache{}, nil
		}
		return nil, err
	}
	var f readmeFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != readmeCacheVersion {
		return ReadmeCache{}, nil
	}
	out := f.Entries
	if out == nil {
		out = ReadmeCache{}
	}
	out.Prune(maxAge)
	return out, nil
}

// Prune drops entries older than maxAge; zero or less keeps every entry.
func (c ReadmeCache) Prune(maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	for name, e := range c {
		if time.Since(e.FetchedAt) > maxAge {
			delete(c, name)
		}
	}
}

// SaveReadmes stores the README openings.
func SaveReadmes(path string, c ReadmeCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(readmeFile{Version: readmeCacheVersion, Entries: c})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ReadmesFromStarCache harvests the READMEs an older version stored inside the
// star cache, so that upgrading does not re-fetch thousands of them. The star
// cache's own age and owner are ignored: a README belongs to a repository, not
// to a particular fetch of somebody's stars.
func ReadmesFromStarCache(path string) ReadmeCache {
	b, err := os.ReadFile(path)
	if err != nil {
		return ReadmeCache{}
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return ReadmeCache{}
	}
	out := ReadmeCache{}
	for _, r := range c.Repos {
		if r.Readme != "" {
			out[r.FullName] = CachedReadme{Text: r.Readme, FetchedAt: c.FetchedAt}
		}
	}
	return out
}
