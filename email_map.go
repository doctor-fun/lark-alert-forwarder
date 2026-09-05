package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EmailMap is the business layer mapping from Feishu open_id to a
// corporate mailbox. We keep it as an interface so broadcast tests can
// stub the map without juggling temp files.
//
// The production implementation is backed by a plain text file (one
// `open_id=email` line per user) mounted from a K8s ConfigMap. Why a
// line file instead of YAML / JSON?
//
//   1. Zero third-party dependencies. A first-party incident path must
//      not fail because gopkg.in/yaml.v3 changed its tag rules.
//   2. ConfigMaps are trivially diffable by humans; line format reviews
//      cleanly in pull requests.
//   3. Empty lines and `#` comments let operators group entries by team
//      and annotate on-call rotations without tooling.
type EmailMap interface {
	Lookup(openID string) string
}

// FileEmailMap loads a newline-delimited `open_id=email` file and
// refreshes it in the background so ConfigMap updates propagate
// without a pod restart. Safe for concurrent use.
type FileEmailMap struct {
	path     string
	interval time.Duration
	// table is stored behind atomic.Value so Lookup never holds a lock;
	// we only take the mutex while reloading from disk.
	table atomic.Value // map[string]string
	// refreshErr records the last reload failure so a misconfigured
	// ConfigMap surfaces in /healthz or ad-hoc logs instead of silently
	// serving stale data. Reads are coordinated via the mutex below.
	mu         sync.Mutex
	refreshErr error
}

// NewFileEmailMap loads `path` once and starts a background refresher
// that re-reads the file every `interval`. Returns early with an error
// when the initial load fails (e.g. bad syntax) so we fatal at startup
// rather than after the first alert. `path == ""` returns a no-op map
// for deployments that haven't opted into the broadcast feature yet.
func NewFileEmailMap(path string, interval time.Duration) (*FileEmailMap, error) {
	fm := &FileEmailMap{path: strings.TrimSpace(path), interval: interval}
	if fm.path == "" {
		fm.table.Store(map[string]string{})
		return fm, nil
	}
	if err := fm.reload(); err != nil {
		return nil, fmt.Errorf("email-map: initial load %s: %w", fm.path, err)
	}
	if interval > 0 {
		go fm.refreshLoop()
	}
	return fm, nil
}

// Lookup returns the email for openID, or "" when not in the map.
// Case-insensitive on the open_id (Feishu sometimes sends mixed case
// in callbacks vs. contact API responses).
func (fm *FileEmailMap) Lookup(openID string) string {
	if fm == nil {
		return ""
	}
	t, _ := fm.table.Load().(map[string]string)
	if t == nil {
		return ""
	}
	return t[strings.ToLower(strings.TrimSpace(openID))]
}

// Size returns the number of entries currently mapped. Handy for
// startup logs and sanity-check endpoints.
func (fm *FileEmailMap) Size() int {
	t, _ := fm.table.Load().(map[string]string)
	return len(t)
}

// LastError returns the error of the most recent reload attempt, or nil
// if the last reload succeeded. Exposed so operational endpoints can
// surface a bad ConfigMap without the pod crashlooping.
func (fm *FileEmailMap) LastError() error {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.refreshErr
}

func (fm *FileEmailMap) refreshLoop() {
	t := time.NewTicker(fm.interval)
	defer t.Stop()
	for range t.C {
		if err := fm.reload(); err != nil {
			fm.mu.Lock()
			fm.refreshErr = err
			fm.mu.Unlock()
		}
	}
}

func (fm *FileEmailMap) reload() error {
	f, err := os.Open(fm.path)
	if err != nil {
		return err
	}
	defer f.Close()
	next, err := parseEmailMap(f)
	if err != nil {
		return err
	}
	fm.table.Store(next)
	fm.mu.Lock()
	fm.refreshErr = nil
	fm.mu.Unlock()
	return nil
}

// parseEmailMap reads one-per-line `open_id=email` pairs. We accept:
//
//   - blank lines (skipped)
//   - comments starting with `#` (skipped)
//   - `key=value` with any amount of whitespace around `=`
//
// We deliberately stop short of validating the email with a regex;
// malformed addresses will be caught by the SMTP server with a clearer
// message than anything we could render at parse time.
func parseEmailMap(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		eq := strings.IndexByte(raw, '=')
		if eq <= 0 || eq == len(raw)-1 {
			return nil, fmt.Errorf("line %d: expected key=value, got %q", line, raw)
		}
		key := strings.ToLower(strings.TrimSpace(raw[:eq]))
		val := strings.TrimSpace(raw[eq+1:])
		if key == "" || val == "" {
			return nil, fmt.Errorf("line %d: empty key or value in %q", line, raw)
		}
		if _, dup := out[key]; dup {
			// A duplicate key usually means a copy/paste error in the
			// ConfigMap; failing loud is safer than silently overriding.
			return nil, fmt.Errorf("line %d: duplicate key %s", line, key)
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// staticEmailMap is a tiny in-memory EmailMap used by tests. Exported
// via the interface only; production never constructs one directly.
type staticEmailMap map[string]string

func (m staticEmailMap) Lookup(openID string) string {
	if m == nil {
		return ""
	}
	return m[strings.ToLower(strings.TrimSpace(openID))]
}
