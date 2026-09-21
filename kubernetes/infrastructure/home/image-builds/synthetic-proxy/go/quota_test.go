package syntheticproxy

import (
	"strings"
	"testing"
)

// TestExhausted pins the fail-over decision: only positive evidence of a
// rate-limited/out-of-quota key may trigger it. A missing ceiling, an absent
// weekly block, or a healthy balance must NOT.
func TestExhausted(t *testing.T) {
	tests := []struct {
		name string
		q    QuotaSnapshot
		want bool
	}{
		{"requests below limit", QuotaSnapshot{Limit: 2000, Requests: 1999}, false},
		{"requests at limit", QuotaSnapshot{Limit: 2000, Requests: 2000}, true},
		{"requests over limit", QuotaSnapshot{Limit: 2000, Requests: 2500}, true},
		{"no ceiling configured is not exhausted", QuotaSnapshot{Limit: 0, Requests: 5}, false},
		{"rolling five hour limited", QuotaSnapshot{Limit: 2000, Requests: 0, RollingLimited: true}, true},
		{"weekly exhausted", QuotaSnapshot{Limit: 2000, HaveWeekly: true, WeeklyPercentRemaining: 0}, true},
		{"weekly negative exhausted", QuotaSnapshot{Limit: 2000, HaveWeekly: true, WeeklyPercentRemaining: -1}, true},
		{"weekly barely remaining", QuotaSnapshot{Limit: 2000, HaveWeekly: true, WeeklyPercentRemaining: 0.1}, false},
		{"weekly absent does not trigger", QuotaSnapshot{Limit: 2000, HaveWeekly: false, WeeklyPercentRemaining: 0}, false},
		{"fully healthy", QuotaSnapshot{Limit: 2000, Requests: 0, RollingLimited: false, HaveWeekly: true, WeeklyPercentRemaining: 60}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.q.Exhausted(); got != tt.want {
				t.Fatalf("Exhausted() = %v, want %v (snapshot %+v)", got, tt.want, tt.q)
			}
		})
	}
}

// TestHashAuthStableAndOpaque guards the no-secret-at-rest property: the stored
// identifier is a fixed-length digest that never reveals the header.
func TestHashAuthStableAndOpaque(t *testing.T) {
	const secret = "Bearer syn_deadbeefdeadbeefdeadbeefdeadbeef"
	h1 := HashAuth(secret)
	h2 := HashAuth(secret)
	if h1 != h2 {
		t.Fatalf("HashAuth not stable: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("HashAuth length = %d, want 64 hex chars", len(h1))
	}
	if strings.Contains(h1, "syn_") || h1 == secret {
		t.Fatalf("HashAuth leaked the credential: %q", h1)
	}
	if HashAuth("Bearer other") == h1 {
		t.Fatal("distinct headers collided")
	}
}

// TestStoreBounded checks the LRU cap actually bounds the map.
func TestStoreBounded(t *testing.T) {
	s := NewStore(3)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		s.Set(k, KeyState{KeyHash: k})
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("Store.Len() = %d, want 3", got)
	}
}
