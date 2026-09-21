package syntheticproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Verdict is the proxy's per-key judgement about Synthetic.
type Verdict int

const (
	// VerdictHealthy: quota remains and Synthetic is reachable -> forward.
	VerdictHealthy Verdict = iota
	// VerdictQuotaExhausted: positive evidence the key is rate-limited or out
	// of quota -> answer 429 so the gateway evicts Synthetic and fails over.
	VerdictQuotaExhausted
	// VerdictUnhealthy: Synthetic is unreachable -> answer 503 (also evicts).
	VerdictUnhealthy
)

func (v Verdict) String() string {
	switch v {
	case VerdictHealthy:
		return "healthy"
	case VerdictQuotaExhausted:
		return "quota_exhausted"
	case VerdictUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// KeyState is the per-API-key state. The RAW key is deliberately absent: the
// map is keyed by KeyHash so a memory dump of this process never yields a
// usable credential.
type KeyState struct {
	// KeyHash is the hex SHA-256 of the raw Authorization header value.
	KeyHash string
	// Verdict is the latest judgement.
	Verdict Verdict
	// RefusedUntil is when the refusal lapses; zero means "not refused".
	RefusedUntil time.Time

	QuotaUsed     int
	QuotaLimit    int
	QuotaRenewsAt time.Time

	RollingLimited         bool
	WeeklyPercentRemaining float64

	// LastCheck drives LRU eviction of the bounded map.
	LastCheck time.Time
}

// HashAuth returns the hex SHA-256 of an Authorization header value. It is the
// only thing the proxy persists about a credential.
func HashAuth(header string) string {
	sum := sha256.Sum256([]byte(header))
	return hex.EncodeToString(sum[:])
}

// Store is a concurrency-safe, bounded, hash-keyed state map. The handler and
// the background poller both touch it, so every method locks.
type Store struct {
	mu   sync.Mutex
	cap  int
	data map[string]KeyState
}

// NewStore returns a Store bounded at cap entries (cap <= 0 is treated as 1).
func NewStore(cap int) *Store {
	if cap < 1 {
		cap = 1
	}
	return &Store{cap: cap, data: make(map[string]KeyState, cap)}
}

// Get returns a copy of the state for a key hash.
func (s *Store) Get(kh string) (KeyState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.data[kh]
	return st, ok
}

// Set inserts or updates a key's state, evicting the entry with the oldest
// LastCheck when the map is at capacity and a new key arrives.
func (s *Store) Set(kh string, st KeyState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[kh]; !exists && len(s.data) >= s.cap {
		s.evictOldestLocked()
	}
	st.KeyHash = kh
	s.data[kh] = st
}

// Delete removes a key's state.
func (s *Store) Delete(kh string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, kh)
}

// Keys returns a snapshot of the currently tracked key hashes (for the poller).
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.data))
	for kh := range s.data {
		out = append(out, kh)
	}
	return out
}

// Len reports the number of tracked keys.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data)
}

// evictOldestLocked drops the least-recently-checked entry. Caller holds mu.
func (s *Store) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for kh, st := range s.data {
		if oldestKey == "" || st.LastCheck.Before(oldest) {
			oldestKey, oldest = kh, st.LastCheck
		}
	}
	if oldestKey != "" {
		delete(s.data, oldestKey)
	}
}
