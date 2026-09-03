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
func SaveCache(path, user string, repos []Repo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(cacheFile{FetchedAt: time.Now(), User: user, Repos: repos})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadCache returns cached stars for the user when they are younger than
// maxAge. A miss is reported as (nil, zero time, nil) rather than an error.
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
	if c.User != user || time.Since(c.FetchedAt) > maxAge {
		return nil, c.FetchedAt, nil
	}
	return c.Repos, c.FetchedAt, nil
}
