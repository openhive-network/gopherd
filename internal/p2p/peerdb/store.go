// Package peerdb implements a persistent address book for the p2p layer.
//
// Each ghive data-dir owns one peers.json that records every endpoint
// the node has heard of (from the static seed list, from address_message
// gossip, or from inbound dialers' advertised inbound port).
//
// Entries carry a small score derived from connect / fail counts so the
// dial loop can prefer recently-active addresses, exponentially back off
// on persistent failures, and never forget a hard-coded seed.
//
// All operations are concurrency-safe. Writes to disk are debounced by
// a background goroutine started via Run; in-memory mutations are
// immediate and observable.
package peerdb

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Filename is the on-disk JSON name under the data-dir.
const Filename = "peers.json"

// Entry is a single tracked endpoint. JSON tags match the on-disk format.
type Entry struct {
	// Endpoint is "host:port" — exactly what the dialer feeds to
	// transport.Dial. Lower-cased for IPv6 literals; ports are decimal
	// strings.
	Endpoint string `json:"endpoint"`
	// Sticky entries (i.e. config-ini seeds) are never garbage-collected
	// nor demoted out of the dial set regardless of failure count.
	Sticky bool `json:"sticky,omitempty"`
	// FirstSeen / LastSeen track when we first heard of and last heard
	// from this endpoint. LastSeen advances on every Touch (typically
	// when an inbound address_info names this endpoint or we successfully
	// connect to it).
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// LastConnected is the wall-clock time of the last successful
	// handshake against this endpoint.
	LastConnected time.Time `json:"last_connected,omitempty"`
	// LastAttempt is the wall-clock time of the last dial we initiated
	// against this endpoint, regardless of outcome.
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	// ConnAttempts / ConnFailures track lifetime totals; failures resets
	// to 0 on a successful connect.
	ConnAttempts int `json:"conn_attempts,omitempty"`
	ConnFailures int `json:"conn_failures,omitempty"`
}

// BackoffSchedule describes the dial back-off curve. Entry i is the
// delay after the i-th consecutive failure (0-indexed). Failures past
// len(Schedule)-1 use the last entry. A zero-length schedule means
// "always retry immediately".
//
// Default mirrors the spec: 1s, 5s, 30s, 5min.
type BackoffSchedule struct {
	Schedule []time.Duration
}

// DefaultBackoff is the schedule documented in the discovery agent spec.
var DefaultBackoff = BackoffSchedule{
	Schedule: []time.Duration{
		1 * time.Second,
		5 * time.Second,
		30 * time.Second,
		5 * time.Minute,
	},
}

// Delay returns how long the dial loop should wait after `failures`
// consecutive failures before retrying.
func (b BackoffSchedule) Delay(failures int) time.Duration {
	if len(b.Schedule) == 0 || failures <= 0 {
		return 0
	}
	if failures > len(b.Schedule) {
		return b.Schedule[len(b.Schedule)-1]
	}
	return b.Schedule[failures-1]
}

// Store is a JSON-backed peer address book.
//
// The zero value is NOT usable; construct via Open or NewMemory.
type Store struct {
	path    string
	backoff BackoffSchedule

	mu      sync.Mutex
	entries map[string]*Entry // keyed by Endpoint
	dirty   bool

	// run-loop control
	persistEvery time.Duration
	stopCh       chan struct{}
	doneCh       chan struct{}
	now          func() time.Time
}

// Options tunes a Store.
type Options struct {
	// PersistEvery is the debounce interval for the background flush.
	// 0 → DefaultPersistEvery (30s). Negative → never flush in the
	// background (caller must Save explicitly).
	PersistEvery time.Duration
	// Backoff overrides the default per-failure dial delay schedule.
	Backoff BackoffSchedule
	// Now is an injectable clock. nil → time.Now.
	Now func() time.Time
}

// DefaultPersistEvery is how often the background goroutine flushes a
// dirty store to disk.
const DefaultPersistEvery = 30 * time.Second

// Open loads the JSON file at path, returning an empty store if it
// doesn't exist yet. The caller should invoke Run to start the
// background flush, then Close on shutdown.
//
// `path` may be empty for a memory-only store (no disk persistence).
func Open(path string, opts Options) (*Store, error) {
	if opts.Backoff.Schedule == nil {
		opts.Backoff = DefaultBackoff
	}
	if opts.PersistEvery == 0 {
		opts.PersistEvery = DefaultPersistEvery
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Store{
		path:         path,
		backoff:      opts.Backoff,
		entries:      make(map[string]*Entry),
		persistEvery: opts.PersistEvery,
		now:          opts.Now,
	}
	if path == "" {
		return s, nil
	}
	if err := s.loadFromDisk(); err != nil {
		return nil, fmt.Errorf("peerdb: load %s: %w", path, err)
	}
	return s, nil
}

// NewMemory returns an in-memory store. Equivalent to Open("", opts)
// but without the error return.
func NewMemory(opts Options) *Store {
	s, _ := Open("", opts)
	return s
}

// loadFromDisk reads the JSON file into s.entries. A missing file is
// not an error.
func (s *Store) loadFromDisk() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	var disk struct {
		Version int      `json:"version"`
		Entries []*Entry `json:"entries"`
	}
	if err := json.Unmarshal(b, &disk); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range disk.Entries {
		if e == nil || e.Endpoint == "" {
			continue
		}
		s.entries[e.Endpoint] = e
	}
	return nil
}

// Save flushes the in-memory state to disk atomically. Safe to call
// from any goroutine. A no-op if path is empty.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	disk := struct {
		Version int      `json:"version"`
		Entries []*Entry `json:"entries"`
	}{
		Version: 1,
		Entries: make([]*Entry, 0, len(s.entries)),
	}
	for _, e := range s.entries {
		// Copy to detach from the live map.
		cp := *e
		disk.Entries = append(disk.Entries, &cp)
	}
	s.dirty = false
	s.mu.Unlock()

	// Stable order helps diffs in version control / debugging.
	sort.Slice(disk.Entries, func(i, j int) bool {
		return disk.Entries[i].Endpoint < disk.Entries[j].Endpoint
	})

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	b, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Run starts a background goroutine that periodically calls Save when
// the store is dirty. Returns immediately. Idempotent — calling Run
// twice is a no-op.
func (s *Store) Run() {
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return
	}
	if s.persistEvery < 0 || s.path == "" {
		s.mu.Unlock()
		return
	}
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	stopCh := s.stopCh
	doneCh := s.doneCh
	period := s.persistEvery
	s.mu.Unlock()
	go func() {
		defer close(doneCh)
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				_ = s.Save()
				return
			case <-t.C:
				_ = s.Save()
			}
		}
	}()
}

// Close stops the background goroutine and flushes one last time.
// Safe to call multiple times.
func (s *Store) Close() error {
	s.mu.Lock()
	stopCh := s.stopCh
	doneCh := s.doneCh
	s.stopCh = nil
	s.mu.Unlock()
	if stopCh != nil {
		close(stopCh)
		<-doneCh
	}
	return s.Save()
}

// AddSticky inserts (or marks) an endpoint as sticky. Sticky entries are
// never demoted out of the dial set or garbage-collected.
func (s *Store) AddSticky(endpoint string) {
	if endpoint == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[endpoint]
	if !ok {
		now := s.now()
		s.entries[endpoint] = &Entry{
			Endpoint:  endpoint,
			Sticky:    true,
			FirstSeen: now,
			LastSeen:  now,
		}
		s.dirty = true
		return
	}
	if !e.Sticky {
		e.Sticky = true
		s.dirty = true
	}
}

// Discover registers an endpoint we just learned about. It's a no-op
// if the endpoint is already known except for the LastSeen bump.
func (s *Store) Discover(endpoint string) {
	if endpoint == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if e, ok := s.entries[endpoint]; ok {
		e.LastSeen = now
		s.dirty = true
		return
	}
	s.entries[endpoint] = &Entry{
		Endpoint:  endpoint,
		FirstSeen: now,
		LastSeen:  now,
	}
	s.dirty = true
}

// MarkAttempt records that we just initiated a dial. Returns the entry
// (creating it if necessary) so the caller can inspect prior state.
func (s *Store) MarkAttempt(endpoint string) {
	if endpoint == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[endpoint]
	if !ok {
		now := s.now()
		e = &Entry{Endpoint: endpoint, FirstSeen: now, LastSeen: now}
		s.entries[endpoint] = e
	}
	e.LastAttempt = s.now()
	e.ConnAttempts++
	s.dirty = true
}

// MarkFailed records a failed dial. Increments the failure counter
// (which feeds the Backoff curve).
func (s *Store) MarkFailed(endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[endpoint]; ok {
		e.ConnFailures++
		s.dirty = true
	}
}

// MarkConnected records a successful handshake. Resets the consecutive
// failure counter so the back-off restarts at delay 0.
func (s *Store) MarkConnected(endpoint string) {
	if endpoint == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e, ok := s.entries[endpoint]
	if !ok {
		e = &Entry{Endpoint: endpoint, FirstSeen: now}
		s.entries[endpoint] = e
	}
	e.LastConnected = now
	e.LastSeen = now
	e.ConnFailures = 0
	s.dirty = true
}

// Get returns a copy of the entry for endpoint, or (Entry{}, false).
func (s *Store) Get(endpoint string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[endpoint]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// All returns a snapshot of every entry.
func (s *Store) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, *e)
	}
	return out
}

// DialableNow returns endpoints whose dial back-off has elapsed and
// which are NOT in the supplied skip set (typically the set of currently-
// connected peers + our own endpoint). Sticky entries always appear,
// even if their back-off has not elapsed (their Delay still applies, but
// we sort them in front so the dial loop sees them first).
//
// Endpoints are returned in priority order: sticky first, then by
// LastSeen descending (recently-active wins), with random shuffling
// inside each tier so we don't pin to a single peer.
func (s *Store) DialableNow(skip map[string]struct{}) []string {
	now := s.now()

	s.mu.Lock()
	tier := struct {
		sticky []*Entry
		other  []*Entry
	}{}
	for _, e := range s.entries {
		if _, blocked := skip[e.Endpoint]; blocked {
			continue
		}
		// Honor back-off — but Sticky entries are never excluded for
		// failure-count reasons, so they go through unconditionally.
		if !e.Sticky {
			delay := s.backoff.Delay(e.ConnFailures)
			if delay > 0 && !e.LastAttempt.IsZero() && now.Sub(e.LastAttempt) < delay {
				continue
			}
		}
		cp := *e
		if cp.Sticky {
			tier.sticky = append(tier.sticky, &cp)
		} else {
			tier.other = append(tier.other, &cp)
		}
	}
	s.mu.Unlock()

	// Sticky tier: prefer least-recently-attempted so the loop fans out.
	sort.SliceStable(tier.sticky, func(i, j int) bool {
		return tier.sticky[i].LastAttempt.Before(tier.sticky[j].LastAttempt)
	})
	// Other tier: prefer most-recently-seen (high-quality), break ties
	// by lowest failure count.
	sort.SliceStable(tier.other, func(i, j int) bool {
		if !tier.other[i].LastSeen.Equal(tier.other[j].LastSeen) {
			return tier.other[i].LastSeen.After(tier.other[j].LastSeen)
		}
		return tier.other[i].ConnFailures < tier.other[j].ConnFailures
	})

	out := make([]string, 0, len(tier.sticky)+len(tier.other))
	for _, e := range tier.sticky {
		out = append(out, e.Endpoint)
	}
	for _, e := range tier.other {
		out = append(out, e.Endpoint)
	}
	return out
}

// Shuffle returns a copy of in with order randomly permuted (Fisher-
// Yates). Useful when the caller wants to spread dial load across
// endpoints with identical priority.
func Shuffle(in []string) []string {
	out := append([]string(nil), in...)
	for i := len(out) - 1; i > 0; i-- {
		j := rand.Intn(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Len returns the number of entries.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
