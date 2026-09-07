package gh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// cacheFile is the on-disk shape of the star cache.
type cacheFile struct {
	FetchedAt time.Time `json:"fetched_at"`
	User      string    `json:"user"`
	Repos     []Repo    `json:"repos"`
}

// DefaultCachePath returns the per-user cache location.
func DefaultCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "constellation-stars.json"
	}
	return filepath.Join(dir, "constellation", "stars.json")
}

// SaveCache stores the fetched stars so that re-running with different
// clustering settings costs no API calls.
//
// README text is stripped: it lives in its own cache with its own expiry, and
// writing it here as well would both double the file and tie a megabyte of
// prose to the star list's one-day lifetime.
func SaveCache(path, user string, repos []Repo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	bare := make([]Repo, len(repos))
	copy(bare, repos)
	for i := range bare {
		bare[i].Readme = ""
	}
	b, err := json.Marshal(cacheFile{FetchedAt: time.Now(), User: user, Repos: bare})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadCache returns cached stars for the user when they are younger than
// maxAge; a maxAge of zero or less accepts the cache at any age, which is what
// an incremental refresh reads it with. A miss is reported as
// (nil, zero time, nil) rather than an error.
func LoadCache(path, user string, maxAge time.Duration) ([]Repo, time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, time.Time{}, nil // a corrupt cache is a miss, not a failure
	}
	if c.User != user || (maxAge > 0 && time.Since(c.FetchedAt) > maxAge) {
		return nil, c.FetchedAt, nil
	}
	return c.Repos, c.FetchedAt, nil
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

// readmeCacheVersion guards the file against a future change of shape. A
// mismatch is treated as a miss, which costs a re-fetch and no correctness.
const readmeCacheVersion = 1

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
