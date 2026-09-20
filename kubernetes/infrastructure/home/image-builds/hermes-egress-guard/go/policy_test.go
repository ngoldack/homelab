// Rule-table tests for the deterministic egress policy engine (port of
// tests/test_policy.py).
//
// These pin the security-relevant contract: the decision ORDER is the
// enforcement boundary, so every table row that a mis-edit could silently widen
// is asserted here.
//
// The engine is deliberately clock-injected and side-effect free, so every test
// drives it with explicit timestamps and a synthetic policy; no wall clock, no
// network, no cluster.
package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPolicyBody is a two-profile policy: an allowlisting profile and an empty
// one. It mirrors the Python _policy() helper.
func testPolicyBody() map[string]any {
	return map[string]any{
		"version": "test-1",
		"profiles": map[string]any{
			"python": map[string]any{
				"allow": []any{
					map[string]any{"host": "pypi.org", "ports": []any{443}},
					map[string]any{"host": ".pythonhosted.org", "ports": []any{443}},
				},
			},
			"offline": map[string]any{"allow": []any{}},
		},
	}
}

// writePolicyFile marshals body into a fresh file and returns its path.
func writePolicyFile(t *testing.T, body any) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

func loadTestPolicy(t *testing.T, body any) *Policy {
	t.Helper()
	policy, err := LoadPolicy(writePolicyFile(t, body))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return policy
}

// newTestEngine loads the standard two-profile policy.
func newTestEngine(t *testing.T) *EgressEngine {
	t.Helper()
	engine, err := NewEgressEngine(loadTestPolicy(t, testPolicyBody()), EgressEngineConfig{
		StrikeThreshold: 3,
		StrikeWindowS:   60.0,
	})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}
	return engine
}

var (
	pythonIdentity  = &Identity{SessionHash: strings.Repeat("s", 64), Profile: "python"}
	offlineIdentity = &Identity{SessionHash: strings.Repeat("o", 64), Profile: "offline"}
)

// --- allow entry matching ---------------------------------------------------

func TestAllowEntryMatchesExactHostAndPort(t *testing.T) {
	entry := AllowEntry{Host: "pypi.org", Ports: map[int]struct{}{443: {}}}

	if !entry.Matches("pypi.org", 443) {
		t.Fatalf("exact host/port did not match")
	}
	if entry.Matches("pypi.org", 80) {
		t.Fatalf("an unlisted port matched")
	}
	if entry.Matches("files.pypi.org", 443) {
		t.Fatalf("an exact entry matched a subdomain")
	}
}

// TestAllowEntrySuffixMatchesSubdomainAndBareDomain pins that a ".suffix" entry
// is a domain-boundary rule, never a substring rule.
func TestAllowEntrySuffixMatchesSubdomainAndBareDomain(t *testing.T) {
	entry := AllowEntry{Host: ".pythonhosted.org", Ports: map[int]struct{}{443: {}}}

	if !entry.Matches("files.pythonhosted.org", 443) {
		t.Fatalf("suffix entry did not match a subdomain")
	}
	if !entry.Matches("pythonhosted.org", 443) {
		t.Fatalf("suffix entry did not match the bare domain")
	}
	if entry.Matches("evilpythonhosted.org", 443) {
		t.Fatalf("suffix entry matched a host without the dot boundary")
	}
	if entry.Matches("files.pythonhosted.org", 80) {
		t.Fatalf("suffix entry matched an unlisted port")
	}
	if entry.Matches("pythonhosted.org.evil.example", 443) {
		t.Fatalf("suffix entry matched a host that only ends with its characters")
	}
}

func TestAllowEntryIsCaseAndTrailingDotInsensitive(t *testing.T) {
	entry := AllowEntry{Host: "pypi.org", Ports: map[int]struct{}{443: {}}}

	if !entry.Matches("PyPI.org.", 443) {
		t.Fatalf("trailing dot/case variants did not match")
	}
	if !entry.Matches("PYPI.ORG", 443) {
		t.Fatalf("upper-case variant did not match")
	}
}

func TestProfilePolicyAllowsIsOrderIndependent(t *testing.T) {
	profile := &ProfilePolicy{
		Name: "p",
		Allow: []AllowEntry{
			{Host: "pypi.org", Ports: map[int]struct{}{443: {}}},
			{Host: ".pythonhosted.org", Ports: map[int]struct{}{443: {}}},
		},
	}

	if !profile.Allows("files.pythonhosted.org", 443) {
		t.Fatalf("second entry did not allow")
	}
	if profile.Allows("files.pythonhosted.org", 80) {
		t.Fatalf("unlisted port was allowed")
	}
}

// --- policy loading ---------------------------------------------------------

func TestLoadPolicyReadsTheShippedSchema(t *testing.T) {
	policy := loadTestPolicy(t, testPolicyBody())

	if policy.Version != "test-1" {
		t.Fatalf("version = %q, want test-1", policy.Version)
	}
	names := policy.ProfileNames()
	if len(names) != 2 || names[0] != "offline" || names[1] != "python" {
		t.Fatalf("profiles = %v, want [offline python]", names)
	}
	// The builtin baseline is always armed, whatever the file says.
	if reason, killed := policy.KillReason("10.0.0.1"); !killed || reason != ReasonKillDestination {
		t.Fatalf("builtin kill baseline not armed: (%q, %v)", reason, killed)
	}
	if reason, killed := policy.KillReason("pypi.org"); killed {
		t.Fatalf("hostname was killed: %q", reason)
	}
}

func TestLoadPolicyNormalizesHosts(t *testing.T) {
	policy := loadTestPolicy(t, map[string]any{
		"version": "1",
		"profiles": map[string]any{
			"a": map[string]any{"allow": []any{map[string]any{"host": " ExAmPle.COM ", "ports": []any{443}}}},
		},
	})
	entry := policy.Profiles["a"].Allow[0]

	if entry.Host != "example.com" {
		t.Fatalf("host = %q, want example.com", entry.Host)
	}
	if !entry.Matches("example.com", 443) {
		t.Fatalf("normalized entry did not match")
	}
}

func TestLoadPolicyMissingFileIsPolicyError(t *testing.T) {
	_, err := LoadPolicy(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatalf("missing file was accepted")
	}
	var policyErr *PolicyError
	if !asPolicyError(err, &policyErr) {
		t.Fatalf("error %v is not a *PolicyError", err)
	}
}

func TestInvalidPoliciesAreRejected(t *testing.T) {
	cases := map[string]any{
		"missing version":      map[string]any{"profiles": map[string]any{"a": map[string]any{"allow": []any{}}}},
		"empty version":        map[string]any{"version": "", "profiles": map[string]any{"a": map[string]any{"allow": []any{}}}},
		"missing profiles":     map[string]any{"version": "1"},
		"empty profiles":       map[string]any{"version": "1", "profiles": map[string]any{}},
		"unknown top key":      map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}}}, "extra": 1},
		"unknown profile key":  map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}, "budget": 1}}},
		"empty ports":          map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x", "ports": []any{}}}}}},
		"port zero":            map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x", "ports": []any{0}}}}}},
		"port too large":       map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x", "ports": []any{65536}}}}}},
		"host type":            map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": 5, "ports": []any{443}}}}}},
		"malformed host":       map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x..", "ports": []any{443}}}}}},
		"non-positive budget":  map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}, "budget_bytes": 0}}},
		"non-int budget":       map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}, "budget_bytes": "100"}}},
		"kill_cidrs not list":  map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}}}, "kill_cidrs": "10.0.0.0/8"},
		"kill_exact not an IP": map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}}}, "kill_exact": []any{"nope"}},
		"unknown allow key":    map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x", "ports": []any{443}, "scheme": "https"}}}}},
		"float port":           map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x", "ports": []any{443.5}}}}}},
		"top level not object": []any{1, 2, 3},
		"kill_cidrs non-string": map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}}}, "kill_cidrs": []any{5}},
		"kill_cidrs not CIDR":   map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}}}, "kill_cidrs": []any{"nope"}},
		"trailing data":         nil, // replaced below; JSON with two documents
		"exponent port":       map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x", "ports": []any{json.Number("443e0")}}}}}},
		"nested port list":    map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x", "ports": []any{[]any{443}}}}}}},
		"null allow entry":    map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{nil}}}},
		"numeric version":     map[string]any{"version": 5, "profiles": map[string]any{"a": map[string]any{"allow": []any{}}}},
		"exponent budget":     map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{}, "budget_bytes": json.Number("1e3")}}},
		"port beyond int64":   map[string]any{"version": "1", "profiles": map[string]any{"a": map[string]any{"allow": []any{map[string]any{"host": "x", "ports": []any{json.Number("99999999999999999999")}}}}}},
	}
	for name, body := range cases {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			path := writePolicyFile(t, body)
			if name == "trailing data" {
				path = filepath.Join(t.TempDir(), "policy.json")
				content := `{"version": "1", "profiles": {"a": {"allow": []}}} {"version": "2"}`
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatalf("write policy: %v", err)
				}
			}
			policy, err := LoadPolicy(path)
			if err == nil {
				t.Fatalf("policy was accepted: %+v", policy)
			}
			var policyErr *PolicyError
			if !asPolicyError(err, &policyErr) {
				t.Fatalf("error %v is not a *PolicyError", err)
			}
		})
	}
}

// TestBudgetBeyondInt64Saturates pins the parity rule for a Python-unbounded
// integer: it is accepted (the Python has no int64 limit), so the budget
// saturates instead of failing startup. No session can reach that byte count, so
// the control stays inert rather than becoming a config error.
func TestBudgetBeyondInt64Saturates(t *testing.T) {
	body := map[string]any{
		"version": "1",
		"profiles": map[string]any{
			"a": map[string]any{"allow": []any{}, "budget_bytes": json.Number("99999999999999999999")},
		},
	}
	policy := loadTestPolicy(t, body)
	budget := policy.Profiles["a"].BudgetBytes
	if budget == nil {
		t.Fatalf("budget was dropped")
	}
	if *budget <= 0 {
		t.Fatalf("budget = %d, want a large positive value", *budget)
	}
}

// asPolicyError reports whether err is a *PolicyError and stores it in target.
func asPolicyError(err error, target **PolicyError) bool {
	policyErr, ok := err.(*PolicyError)
	if ok {
		*target = policyErr
	}
	return ok
}

// --- kill baseline and policy kill lists ------------------------------------

func TestBuiltinKillDestinations(t *testing.T) {
	engine := newTestEngine(t)

	hosts := []string{
		"169.254.169.254", // cloud metadata, named in the plan
		"10.1.2.3",        // RFC1918
		"192.168.1.1",     // RFC1918
		"127.0.0.1",       // loopback
		"100.64.0.1",      // CGNAT
		"::1",             // IPv6 loopback
		"fd00::1",         // IPv6 ULA
		"172.16.9.9",      // RFC1918
		"224.0.0.1",       // multicast
		"0.0.0.0",         // unspecified
		"fe80::1",         // IPv6 link-local
		"ff02::1",         // IPv6 multicast
	}
	for _, host := range hosts {
		decision := engine.EvaluateAt(pythonIdentity, host, 443, 0, 0)
		if decision.Kind != Kill || decision.Reason != ReasonKillDestination || decision.Status != 403 {
			t.Fatalf("%s: got (%s, %s, %d), want (kill, kill-destination, 403)", host, decision.Kind, decision.Reason, decision.Status)
		}
	}
}

// TestKillWinsOverAnAllowlistEntry: even a policy that tries to allowlist the
// management plane loses.
func TestKillWinsOverAnAllowlistEntry(t *testing.T) {
	policy := loadTestPolicy(t, map[string]any{
		"version": "test-1",
		"profiles": map[string]any{
			"python": map[string]any{"allow": []any{map[string]any{"host": "10.0.0.5", "ports": []any{443}}}},
		},
	})
	engine, err := NewEgressEngine(policy, EgressEngineConfig{})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}

	if decision := engine.EvaluateAt(pythonIdentity, "10.0.0.5", 443, 0, 0); decision.Kind != Kill {
		t.Fatalf("kind = %s, want kill", decision.Kind)
	}
}

func TestPolicyKillCidrsExtendTheBuiltinBaseline(t *testing.T) {
	body := testPolicyBody()
	body["kill_cidrs"] = []any{"172.20.0.0/16"}
	body["kill_exact"] = []any{"203.0.113.7"}
	engine, err := NewEgressEngine(loadTestPolicy(t, body), EgressEngineConfig{})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}

	if decision := engine.EvaluateAt(pythonIdentity, "172.20.5.5", 443, 0, 0); decision.Kind != Kill {
		t.Fatalf("policy CIDR: kind = %s, want kill", decision.Kind)
	}
	if decision := engine.EvaluateAt(pythonIdentity, "203.0.113.7", 443, 0, 0); decision.Kind != Kill {
		t.Fatalf("policy exact: kind = %s, want kill", decision.Kind)
	}
	// A hostname is never resolved here (DNS-rebinding defence is Cilium's).
	if decision := engine.EvaluateAt(pythonIdentity, "pypi.org", 443, 0, 0); decision.Kind != Allow {
		t.Fatalf("hostname: kind = %s, want allow", decision.Kind)
	}
}

// TestKillCidrParserFollowsIpaddressSemantics pins the two ipaddress.ip_network
// behaviours a hand-written parser gets wrong: a bare address is a host route,
// and host bits below the prefix are dropped rather than rejected.
func TestKillCidrParserFollowsIpaddressSemantics(t *testing.T) {
	body := testPolicyBody()
	body["kill_cidrs"] = []any{"10.0.0.1/8", "203.0.113.7", "2001:db8::1"}
	policy := loadTestPolicy(t, body)

	if len(policy.KillCIDRs) != len(BuiltinKillCIDRs)+3 {
		t.Fatalf("networks = %d, want %d", len(policy.KillCIDRs), len(BuiltinKillCIDRs)+3)
	}
	// "10.0.0.1/8" is stored masked: it kills the whole /8.
	if reason, killed := policy.KillReason("10.9.9.9"); !killed || reason != ReasonKillDestination {
		t.Fatalf("host bits were not dropped: (%q, %v)", reason, killed)
	}
	// A bare address is a /32 host route — and only that address.
	if _, killed := policy.KillReason("203.0.113.7"); !killed {
		t.Fatalf("bare IPv4 address was not treated as a host route")
	}
	if _, killed := policy.KillReason("203.0.113.8"); killed {
		t.Fatalf("bare IPv4 address leaked into its neighbours")
	}
	if _, killed := policy.KillReason("2001:db8::1"); !killed {
		t.Fatalf("bare IPv6 address was not treated as a host route")
	}
	if _, killed := policy.KillReason("2001:db8::2"); killed {
		t.Fatalf("bare IPv6 address leaked into its neighbours")
	}
}

// TestKillExactAcceptsOnlyAddressLiterals pins that kill_exact takes addresses,
// never CIDRs (the Python's ip_address, which unlike ip_network rejects a "/").
func TestKillExactAcceptsOnlyAddressLiterals(t *testing.T) {
	body := testPolicyBody()
	body["kill_exact"] = []any{"203.0.113.7/32"}
	if _, err := LoadPolicy(writePolicyFile(t, body)); err == nil {
		t.Fatalf("a CIDR in kill_exact was accepted")
	}
}

// TestKillCidrsParseBothFamilies pins IPv4 and IPv6 CIDR parsing and the family
// guard: an IPv6 network never matches an IPv4 address.
func TestKillCidrsParseBothFamilies(t *testing.T) {
	body := testPolicyBody()
	body["kill_cidrs"] = []any{"2001:db8::/32", "198.51.100.0/24"}
	engine, err := NewEgressEngine(loadTestPolicy(t, body), EgressEngineConfig{})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}

	// Each assertion gets its own session: a kill counts a strike, and three
	// strikes escalate the next verdict to a kill on their own.
	if decision := engine.EvaluateAt(sessionIdentity("v6"), "2001:db8::5", 443, 0, 0); decision.Kind != Kill {
		t.Fatalf("IPv6 CIDR: kind = %s, want kill", decision.Kind)
	}
	if decision := engine.EvaluateAt(sessionIdentity("v4"), "198.51.100.9", 443, 0, 0); decision.Kind != Kill {
		t.Fatalf("IPv4 CIDR: kind = %s, want kill", decision.Kind)
	}
	// Outside both added networks and not otherwise killed.
	if decision := engine.EvaluateAt(sessionIdentity("out"), "203.0.113.9", 443, 0, 0); decision.Kind != Deny {
		t.Fatalf("outside CIDR: kind = %s, want deny", decision.Kind)
	}
	// The boundary of the IPv6 network: 2001:db9:: is outside 2001:db8::/32.
	if decision := engine.EvaluateAt(sessionIdentity("v6out"), "2001:db9::5", 443, 0, 0); decision.Kind != Deny {
		t.Fatalf("outside IPv6 CIDR: kind = %s, want deny", decision.Kind)
	}
	if decision := engine.EvaluateAt(sessionIdentity("allow"), "pypi.org", 443, 0, 0); decision.Kind != Allow {
		t.Fatalf("hostname: kind = %s, want allow", decision.Kind)
	}
}

// sessionIdentity builds a fresh python-profile identity per session label, so
// one assertion's strikes cannot leak into the next.
func sessionIdentity(label string) *Identity {
	return &Identity{SessionHash: strings.Repeat("s", 64-len(label)) + label, Profile: "python"}
}

func TestHostnameThatResemblesAnIPIsNotKilled(t *testing.T) {
	engine := newTestEngine(t)

	// Only IP literals take the kill path; a name is judged by the allowlist.
	if decision := engine.EvaluateAt(pythonIdentity, "10.0.0.1.example.com", 443, 0, 0); decision.Kind != Deny {
		t.Fatalf("kind = %s, want deny", decision.Kind)
	}
}

func TestIPv4MappedIPv6KillAddressIsCaught(t *testing.T) {
	engine := newTestEngine(t)

	if decision := engine.EvaluateAt(pythonIdentity, "::ffff:169.254.169.254", 443, 0, 0); decision.Kind != Kill {
		t.Fatalf("kind = %s, want kill", decision.Kind)
	}
}

func TestBracketedIPv6LiteralIsKilled(t *testing.T) {
	engine := newTestEngine(t)

	if decision := engine.EvaluateAt(pythonIdentity, "[fd00::1]", 443, 0, 0); decision.Kind != Kill {
		t.Fatalf("bracketed literal: kind = %s, want kill", decision.Kind)
	}
}

// --- decision order ---------------------------------------------------------

func TestUnauthenticatedIsDeniedWithoutStrike(t *testing.T) {
	engine := newTestEngine(t)

	first := engine.EvaluateAt(nil, "pypi.org", 443, 0, 0)
	second := engine.EvaluateAt(nil, "pypi.org", 443, 0, 0)

	if first.Kind != Deny || first.Reason != ReasonUnauthenticated || first.Status != 403 {
		t.Fatalf("first = (%s, %s, %d), want (deny, unauthenticated, 403)", first.Kind, first.Reason, first.Status)
	}
	if second.Kind != Deny || second.Strikes != 0 {
		t.Fatalf("second = (%s, %d), want (deny, 0)", second.Kind, second.Strikes)
	}
}

func TestUnknownProfileIsDeniedWithoutStrike(t *testing.T) {
	engine := newTestEngine(t)
	stranger := &Identity{SessionHash: strings.Repeat("x", 64), Profile: "ruby"}

	for tick := 0; tick < 5; tick++ {
		decision := engine.EvaluateAt(stranger, "pypi.org", 443, 0, float64(tick))
		if decision.Kind != Deny || decision.Reason != ReasonProfileUnknown {
			t.Fatalf("tick %d: got (%s, %s), want (deny, profile-unknown)", tick, decision.Kind, decision.Reason)
		}
		if decision.Strikes != 0 {
			t.Fatalf("tick %d: strikes = %d, want 0", tick, decision.Strikes)
		}
	}
}

func TestAllowlistedHostIsAllowed(t *testing.T) {
	engine := newTestEngine(t)

	decision := engine.EvaluateAt(pythonIdentity, "pypi.org", 443, 0, 0)
	if decision.Kind != Allow || decision.Reason != ReasonAllowed || decision.Status != 200 {
		t.Fatalf("got (%s, %s, %d), want (allow, allowed, 200)", decision.Kind, decision.Reason, decision.Status)
	}
}

func TestHostMatchingIsCaseAndTrailingDotInsensitive(t *testing.T) {
	engine := newTestEngine(t)

	if decision := engine.EvaluateAt(pythonIdentity, "PyPI.org.", 443, 0, 0); decision.Kind != Allow {
		t.Fatalf("kind = %s, want allow", decision.Kind)
	}
}

func TestPortMustBeAllowlisted(t *testing.T) {
	engine := newTestEngine(t)

	decision := engine.EvaluateAt(pythonIdentity, "pypi.org", 22, 0, 0)
	if decision.Kind != Deny || decision.Reason != ReasonNotAllowlisted {
		t.Fatalf("got (%s, %s), want (deny, not-allowlisted)", decision.Kind, decision.Reason)
	}
}

func TestEmptyAllowlistProfileDeniesEverything(t *testing.T) {
	engine := newTestEngine(t)

	if decision := engine.EvaluateAt(offlineIdentity, "pypi.org", 443, 0, 0); decision.Kind != Deny {
		t.Fatalf("kind = %s, want deny", decision.Kind)
	}
}

// --- strikes and escalation -------------------------------------------------

func TestDenialsAccumulateStrikesAndTheThresholdKills(t *testing.T) {
	engine := newTestEngine(t)

	first := engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 0)
	second := engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 1)
	third := engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 2)

	if first.Kind != Deny || first.Strikes != 1 {
		t.Fatalf("first = (%s, %d), want (deny, 1)", first.Kind, first.Strikes)
	}
	if second.Kind != Deny || second.Strikes != 2 {
		t.Fatalf("second = (%s, %d), want (deny, 2)", second.Kind, second.Strikes)
	}
	// The strike that reaches the threshold escalates the response itself.
	if third.Kind != Kill || third.Reason != ReasonStrikes || third.Strikes != 3 {
		t.Fatalf("third = (%s, %s, %d), want (kill, strikes, 3)", third.Kind, third.Reason, third.Strikes)
	}
}

func TestStrikesExpireOutsideTheWindow(t *testing.T) {
	engine := newTestEngine(t)

	engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 0)
	engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 30)

	// A 60s window measured back from 91 discards both earlier strikes (0 and 30
	// are < 31), so this denial starts a fresh count.
	decision := engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 91)
	if decision.Kind != Deny || decision.Strikes != 1 {
		t.Fatalf("got (%s, %d), want (deny, 1)", decision.Kind, decision.Strikes)
	}
}

func TestStrikesArePerSession(t *testing.T) {
	engine := newTestEngine(t)
	other := &Identity{SessionHash: strings.Repeat("p", 64), Profile: "python"}

	engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 0)
	engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 0)

	if decision := engine.EvaluateAt(other, "evil.example", 443, 0, 0); decision.Strikes != 1 {
		t.Fatalf("strikes = %d, want 1", decision.Strikes)
	}
}

func TestAllowedRequestDoesNotStrike(t *testing.T) {
	engine := newTestEngine(t)

	engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 0)
	allowed := engine.EvaluateAt(pythonIdentity, "pypi.org", 443, 0, 0)
	after := engine.EvaluateAt(pythonIdentity, "evil.example", 443, 0, 0)

	if allowed.Kind != Allow {
		t.Fatalf("kind = %s, want allow", allowed.Kind)
	}
	if after.Strikes != 2 { // the allow neither added nor cleared a strike
		t.Fatalf("strikes = %d, want 2", after.Strikes)
	}
}

func TestKillDestinationAlsoStrikes(t *testing.T) {
	// An instant-kill target is still counted, so the reaper sees escalation.
	engine := newTestEngine(t)

	first := engine.EvaluateAt(pythonIdentity, "169.254.169.254", 443, 0, 0)
	second := engine.EvaluateAt(pythonIdentity, "169.254.169.254", 443, 0, 0)

	if first.Kind != Kill || first.Strikes != 1 {
		t.Fatalf("first = (%s, %d), want (kill, 1)", first.Kind, first.Strikes)
	}
	if second.Strikes != 2 {
		t.Fatalf("second strikes = %d, want 2", second.Strikes)
	}
}

func TestSessionTableIsBounded(t *testing.T) {
	engine := newTestEngine(t)

	for index := 0; index < engine.MaxSessions+50; index++ {
		identity := &Identity{
			SessionHash: padNumeric(index, 64),
			Profile:     "python",
		}
		engine.EvaluateAt(identity, "pypi.org", 443, 0, float64(index))
	}

	if seen := engine.SessionsSeen(); seen > engine.MaxSessions {
		t.Fatalf("sessions = %d, want <= %d", seen, engine.MaxSessions)
	}
}

// padNumeric renders index zero-padded to width, mirroring the Python f"{index:064d}".
func padNumeric(index int, width int) string {
	digits := []byte{}
	if index == 0 {
		digits = []byte("0")
	}
	for value := index; value > 0; value /= 10 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
	}
	for len(digits) < width {
		digits = append([]byte("0"), digits...)
	}
	return string(digits)
}

// --- byte budget ------------------------------------------------------------

func TestByteBudgetExhaustionDenies(t *testing.T) {
	engine, err := NewEgressEngine(loadTestPolicy(t, map[string]any{
		"version": "test-1",
		"profiles": map[string]any{
			"python": map[string]any{
				"allow":        []any{map[string]any{"host": "pypi.org", "ports": []any{443}}},
				"budget_bytes": 1000,
			},
		},
	}), EgressEngineConfig{})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}

	if decision := engine.EvaluateAt(pythonIdentity, "pypi.org", 443, 600, 0); decision.Kind != Allow {
		t.Fatalf("first: kind = %s, want allow", decision.Kind)
	}
	if decision := engine.EvaluateAt(pythonIdentity, "pypi.org", 443, 600, 0); decision.Kind != Deny {
		t.Fatalf("second: kind = %s, want deny", decision.Kind)
	}
	exhausted := engine.EvaluateAt(pythonIdentity, "pypi.org", 443, 1, 0)
	if exhausted.Kind != Deny || exhausted.Reason != ReasonBudgetExhausted {
		t.Fatalf("third = (%s, %s), want (deny, budget-exhausted)", exhausted.Kind, exhausted.Reason)
	}
}

func TestBudgetWithoutHintIsNotConsumed(t *testing.T) {
	engine, err := NewEgressEngine(loadTestPolicy(t, map[string]any{
		"version": "test-1",
		"profiles": map[string]any{
			"python": map[string]any{
				"allow":        []any{map[string]any{"host": "pypi.org", "ports": []any{443}}},
				"budget_bytes": 1000,
			},
		},
	}), EgressEngineConfig{})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}

	for index := 0; index < 10; index++ {
		if decision := engine.EvaluateAt(pythonIdentity, "pypi.org", 443, 0, 0); decision.Kind != Allow {
			t.Fatalf("iteration %d: kind = %s, want allow", index, decision.Kind)
		}
	}
}

// --- engine arguments -------------------------------------------------------

func TestEngineDefaultsMatchTheDeployedContract(t *testing.T) {
	engine, err := NewEgressEngine(loadTestPolicy(t, testPolicyBody()), EgressEngineConfig{})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}

	if engine.StrikeThreshold != 3 || engine.StrikeWindowS != 60.0 || engine.MaxSessions != 4096 {
		t.Fatalf("defaults = (%d, %v, %d), want (3, 60, 4096)", engine.StrikeThreshold, engine.StrikeWindowS, engine.MaxSessions)
	}
}

func TestNewEgressEngineRejectsNegativeKnobs(t *testing.T) {
	policy := loadTestPolicy(t, testPolicyBody())

	if _, err := NewEgressEngine(policy, EgressEngineConfig{StrikeThreshold: -1}); err == nil {
		t.Fatalf("negative strike threshold was accepted")
	}
	if _, err := NewEgressEngine(policy, EgressEngineConfig{StrikeWindowS: -1}); err == nil {
		t.Fatalf("negative strike window was accepted")
	}
	if _, err := NewEgressEngine(policy, EgressEngineConfig{MaxSessions: -1}); err == nil {
		t.Fatalf("negative max sessions was accepted")
	}
}