// Deterministic egress policy engine (port of src/egress_guard/policy.py).
//
// The policy is JSON loaded from EGRESS_POLICY_FILE (mounted from the
// egress-authorizer-policy ConfigMap, key policy.json). Nothing about a profile
// is embedded in this code: an unknown profile is a denial, never an implicit
// allow, so a ConfigMap edit is the only way to widen egress.
//
// Decision order (deterministic, no DNS, no clock-dependent branching beyond the
// strike window):
//
//  1. unauthenticated -> deny "unauthenticated" (no strike: there is no session
//     to count against).
//  2. instant-kill destinations -> kill "kill-destination". Only IP-literal
//     targets are evaluated; a hostname is never resolved here (the authorizer
//     makes no network calls, and DNS-rebinding defence is Cilium's job).
//  3. unknown profile -> deny "profile-unknown" (no strike: a policy mistake,
//     not sandbox behaviour).
//  4. host/port not allowlisted -> deny "not-allowlisted" (strike; the strike
//     that reaches the threshold escalates the same response to a kill).
//  5. byte budget exceeded -> deny "budget-exhausted" (strike, same escalation).
//
// Strike accounting: EGRESS_STRIKE_THRESHOLD denies inside
// EGRESS_STRIKE_WINDOW_S seconds => quarantine. Strikes are per session hash and
// in-memory only, bounded by EGRESS_MAX_SESSIONS (LRU by last use), so a restart
// forgets strikes but never the quarantine ConfigMap.
package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Decision kinds, carried on x-egress-decision and in the check body.
const (
	// Allow authorizes the request (HTTP 200).
	Allow = "allow"
	// Deny refuses the request (HTTP 403) and counts a strike.
	Deny = "deny"
	// Kill refuses the request (HTTP 403), sets x-egress-kill and quarantines
	// the session through the reaper.
	Kill = "kill"
)

// Decision reasons, carried on x-egress-reason and in the emitted event.
const (
	// ReasonAllowed is the reason of an Allow decision.
	ReasonAllowed = "allowed"
	// ReasonUnauthenticated: no verified identity; no strike.
	ReasonUnauthenticated = "unauthenticated"
	// ReasonKillDestination: an IP literal inside the kill baseline or the
	// policy's kill lists.
	ReasonKillDestination = "kill-destination"
	// ReasonProfileUnknown: the token named a profile the policy does not
	// define; no strike.
	ReasonProfileUnknown = "profile-unknown"
	// ReasonNotAllowlisted: host/port outside the profile's allowlist.
	ReasonNotAllowlisted = "not-allowlisted"
	// ReasonBudgetExhausted: the profile's byte budget was exceeded.
	ReasonBudgetExhausted = "budget-exhausted"
	// ReasonStrikes: the denial that reached the strike threshold, escalated to
	// a kill.
	ReasonStrikes = "strikes"
)

// EgressEngine defaults; they mirror the Python keyword defaults and are part of
// the deployed contract.
const (
	// DefaultStrikeThreshold is EGRESS_STRIKE_THRESHOLD's fallback.
	DefaultStrikeThreshold = 3
	// DefaultStrikeWindowS is EGRESS_STRIKE_WINDOW_S's fallback.
	DefaultStrikeWindowS = 60.0
	// DefaultMaxSessions is EGRESS_MAX_SESSIONS's fallback.
	DefaultMaxSessions = 4096
)

// BuiltinKillCIDRs is the instant-kill baseline, independent of the policy file:
// RFC1918, loopback, link-local (including the 169.254.169.254 cloud metadata
// address), CGNAT, unspecified/multicast/reserved space and their IPv6
// counterparts. These are never reachable through the guard even if a policy
// file were to allowlist them, so a compromised/misconfigured policy cannot
// re-open the management plane. Cluster-specific ranges (pod/service CIDR, node
// subnet, management networks) are added by the policy file's kill_cidrs.
var BuiltinKillCIDRs = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
}

// BuiltinKillExact is the always-kill addresses by value, on top of the ranges
// above. 169.254.169.254 is already inside 169.254.0.0/16; it is listed
// explicitly because it is the metadata endpoint the plan calls out by name (and
// because an operator reading the policy should see it).
var BuiltinKillExact = []string{"169.254.169.254"}

// PolicyError is the mount-read failure mode: the policy file is unreadable,
// unparseable or violates the schema. It is the Go counterpart of the Python
// PolicyError, which subclasses ValueError so every caller's parse-error path
// catches it.
type PolicyError struct {
	Message string
}

func (e *PolicyError) Error() string { return e.Message }

// NewPolicyError formats a PolicyError; the format string mirrors the Python
// f-strings so an operator sees the same text in the log.
func NewPolicyError(format string, args ...any) *PolicyError {
	return &PolicyError{Message: fmt.Sprintf(format, args...)}
}

// AllowEntry is one allowlist entry: an exact host or a ".suffix" domain, plus
// the ports it is reachable on.
type AllowEntry struct {
	Host  string
	Ports map[int]struct{}
}

// Matches reports whether host:port is covered by this entry. The comparison is
// case- and trailing-dot-insensitive (the Python lower().rstrip(".")), and a
// ".suffix" entry matches the bare domain as well as any subdomain — but never a
// host that merely ends with the same characters without the dot boundary.
func (e *AllowEntry) Matches(host string, port int) bool {
	if _, ok := e.Ports[port]; !ok {
		return false
	}
	normalized := strings.TrimRight(strings.ToLower(host), ".")
	if strings.HasPrefix(e.Host, ".") {
		bare := e.Host[1:]
		return normalized == bare || strings.HasSuffix(normalized, e.Host)
	}
	return normalized == e.Host
}

// ProfilePolicy is one egress profile: its name, its allowlist and, optionally, a
// byte budget. The budget only fires when the caller supplies a byte_hint > 0
// (see README: real byte accounting needs an Envoy-side reporter).
type ProfilePolicy struct {
	Name        string
	Allow       []AllowEntry
	BudgetBytes *int
}

// Allows reports whether any allowlist entry covers host:port.
func (p *ProfilePolicy) Allows(host string, port int) bool {
	for index := range p.Allow {
		if p.Allow[index].Matches(host, port) {
			return true
		}
	}
	return false
}

// Policy is a validated policy file plus the always-armed builtin kill baseline.
type Policy struct {
	Version   string
	Profiles  map[string]*ProfilePolicy
	KillCIDRs []netip.Prefix
	KillExact map[netip.Addr]struct{}
}

// KillReason returns ReasonKillDestination when host is an IP literal the guard
// never reaches out to, else ("", false). Hostnames are not resolved.
//
// An IPv4-mapped IPv6 literal is judged both as written and unmapped, so
// ::ffff:169.254.169.254 is killed by an IPv4 rule. A range only ever matches an
// address of its own family, matching the Python's explicit version guard.
func (p *Policy) KillReason(host string) (string, bool) {
	address, ok := parsePolicyIP(host)
	if !ok {
		return "", false
	}
	candidates := []netip.Addr{address}
	if address.Is4In6() {
		if unmapped := address.Unmap(); unmapped != address {
			candidates = append(candidates, unmapped)
		}
	}
	for _, candidate := range candidates {
		if _, ok := p.KillExact[candidate]; ok {
			return ReasonKillDestination, true
		}
		for _, network := range p.KillCIDRs {
			if network.Contains(candidate) {
				return ReasonKillDestination, true
			}
		}
	}
	return "", false
}

// ProfileNames returns the profile names in sorted order, which is the shape
// the /healthz body publishes (Python: sorted(policy.profiles)).
func (p *Policy) ProfileNames() []string {
	names := make([]string, 0, len(p.Profiles))
	for name := range p.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LoadPolicy reads and validates the policy file. Every failure — an unreadable
// path, malformed JSON, a schema violation — is a *PolicyError, so a bad mount
// fails the authorizer at startup instead of silently allowing egress.
func LoadPolicy(path string) (*Policy, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, NewPolicyError("policy: cannot read %s: %v", path, err)
	}
	defer handle.Close()

	// UseNumber keeps the decoder's view of a number as text, so an integer can
	// be told from a float exactly as Python's json.load does (a float port is
	// rejected there and here).
	decoder := json.NewDecoder(handle)
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		return nil, NewPolicyError("policy: invalid JSON: %v", err)
	}
	// Python's json.load rejects trailing content; a decoder would happily stop
	// after the first value and ignore a truncated second document.
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, NewPolicyError("policy: invalid JSON: extra data after the top-level object")
	}

	document, ok := raw.(map[string]any)
	if !ok {
		return nil, NewPolicyError("policy: expected a JSON object at the top level")
	}
	if err := rejectUnknownKeys(document, []string{"version", "profiles", "kill_cidrs", "kill_exact"}, "policy"); err != nil {
		return nil, err
	}

	version, ok := document["version"].(string)
	if !ok || strings.TrimSpace(version) == "" {
		return nil, NewPolicyError("policy.version: expected a non-empty string")
	}

	rawProfiles, ok := document["profiles"].(map[string]any)
	if !ok || len(rawProfiles) == 0 {
		return nil, NewPolicyError("policy.profiles: expected a non-empty object")
	}
	profiles := make(map[string]*ProfilePolicy, len(rawProfiles))
	for _, name := range sortedObjectKeys(rawProfiles) {
		profile, err := parseProfilePolicy(name, rawProfiles[name])
		if err != nil {
			return nil, err
		}
		profiles[name] = profile
	}

	networks, err := parseKillCIDRs(BuiltinKillCIDRs, "builtin-kill-cidrs", nil)
	if err != nil {
		return nil, err
	}
	rawCIDRs, err := objectList(document, "kill_cidrs")
	if err != nil {
		return nil, err
	}
	networkTexts, err := textList(rawCIDRs, "kill_cidrs")
	if err != nil {
		return nil, err
	}
	networks, err = parseKillCIDRs(networkTexts, "kill_cidrs", networks)
	if err != nil {
		return nil, err
	}

	exact := make(map[netip.Addr]struct{}, len(BuiltinKillExact))
	if err := addKillExact(BuiltinKillExact, "builtin-kill-exact", exact); err != nil {
		return nil, err
	}
	rawExact, err := objectList(document, "kill_exact")
	if err != nil {
		return nil, err
	}
	exactTexts, err := textList(rawExact, "kill_exact")
	if err != nil {
		return nil, err
	}
	if err := addKillExact(exactTexts, "kill_exact", exact); err != nil {
		return nil, err
	}

	return &Policy{
		Version:   version,
		Profiles:  profiles,
		KillCIDRs: networks,
		KillExact: exact,
	}, nil
}

// parseProfilePolicy validates one profile object: its allowlist entries (host +
// non-empty port list) and its optional positive byte budget.
func parseProfilePolicy(name string, raw any) (*ProfilePolicy, error) {
	where := "policy.profiles." + name
	body, ok := raw.(map[string]any)
	if !ok {
		return nil, NewPolicyError("%s: expected an object", where)
	}
	if err := rejectUnknownKeys(body, []string{"allow", "budget_bytes"}, where); err != nil {
		return nil, err
	}

	rawAllow, present := body["allow"]
	if !present {
		rawAllow = []any{}
	}
	allowList, ok := rawAllow.([]any)
	if !ok {
		return nil, NewPolicyError("%s.allow: expected a list", where)
	}

	entries := make([]AllowEntry, 0, len(allowList))
	for index, rawEntry := range allowList {
		entryWhere := fmt.Sprintf("%s.allow[%d]", where, index)
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return nil, NewPolicyError("%s: expected an object", entryWhere)
		}
		if err := rejectUnknownKeys(entry, []string{"host", "ports"}, entryWhere); err != nil {
			return nil, err
		}
		rawHost, ok := entry["host"].(string)
		if !ok || strings.TrimSpace(rawHost) == "" {
			return nil, NewPolicyError("%s.host: expected a non-empty string", entryWhere)
		}
		host := strings.ToLower(strings.TrimSpace(rawHost))
		if strings.HasSuffix(host, "..") {
			return nil, NewPolicyError("%s.host: malformed host %s", entryWhere, policyRepr(host))
		}
		rawPorts, present := entry["ports"]
		portList, ok := rawPorts.([]any)
		if !present || !ok || len(portList) == 0 {
			return nil, NewPolicyError("%s.ports: expected a non-empty list", entryWhere)
		}
		ports := make(map[int]struct{}, len(portList))
		for _, rawPort := range portList {
			port, err := parsePolicyPort(rawPort, entryWhere+".ports", name)
			if err != nil {
				return nil, err
			}
			ports[port] = struct{}{}
		}
		entries = append(entries, AllowEntry{Host: host, Ports: ports})
	}

	var budget *int
	if rawBudget, present := body["budget_bytes"]; present && rawBudget != nil {
		value, err := asPolicyInt(rawBudget, where+".budget_bytes")
		if err != nil {
			return nil, err
		}
		if value <= 0 {
			return nil, NewPolicyError("%s.budget_bytes: must be positive", where)
		}
		budget = &value
	}

	return &ProfilePolicy{Name: name, Allow: entries, BudgetBytes: budget}, nil
}

// parseKillCIDRs appends the parsed networks to accumulated. A bare address is a
// host route (/32 or /128) and host bits below the prefix are dropped — both
// exactly what the Python does with ipaddress.ip_network(cidr, strict=False).
//
// Deviation, deliberately stricter: the Python's ip_network also coerces a JSON
// integer element into an address (5 -> 0.0.0.5/32), because ipaddress accepts
// any int it can pack. textList has already required strings, so a numeric
// element is a schema error here where the Python silently accepted it. That can
// only turn a misconfigured kill list into a loud startup failure, never into a
// reachable destination.
func parseKillCIDRs(values []string, key string, accumulated []netip.Prefix) ([]netip.Prefix, error) {
	for _, text := range values {
		if prefix, err := netip.ParsePrefix(text); err == nil {
			accumulated = append(accumulated, prefix.Masked())
			continue
		}
		if address, err := netip.ParseAddr(text); err == nil {
			accumulated = append(accumulated, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		return nil, NewPolicyError("policy.%s: not a CIDR network: %s", key, policyRepr(text))
	}
	return accumulated, nil
}

// addKillExact parses address literals into target.
func addKillExact(values []string, key string, target map[netip.Addr]struct{}) error {
	for _, text := range values {
		address, err := netip.ParseAddr(text)
		if err != nil {
			return NewPolicyError("policy.%s: not an IP address: %s", key, policyRepr(text))
		}
		target[address] = struct{}{}
	}
	return nil
}

// textList validates a decoded list field as a list of strings. A non-list value
// or a non-string element is the schema violation the Python's isinstance checks
// report.
func textList(values []any, key string) ([]string, error) {
	texts := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, NewPolicyError("policy.%s: expected a string, got %s", key, policyRepr(value))
		}
		texts = append(texts, text)
	}
	return texts, nil
}

// objectList reads an optional top-level list field.
func objectList(document map[string]any, key string) ([]any, error) {
	raw, present := document[key]
	if !present {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, NewPolicyError("policy.%s: expected a list", key)
	}
	return values, nil
}

// rejectUnknownKeys fails on any key outside allowed, so a typo in the ConfigMap
// ("allow_hosts") is a startup failure rather than a silent narrowing.
func rejectUnknownKeys(object map[string]any, allowed []string, where string) error {
	permitted := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		permitted[key] = struct{}{}
	}
	var unknown []string
	for key := range object {
		if _, ok := permitted[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	permittedList := append([]string(nil), allowed...)
	sort.Strings(permittedList)
	return NewPolicyError("%s: unknown key(s) %s; allowed: %s", where, policyReprList(unknown), policyReprList(permittedList))
}

// asPolicyInt accepts only a JSON number with no fractional part. Python's
// json.load yields int for those and float for the rest, and _as_int rejects the
// float — including a bool, which is an int subclass there.
//
// An integer beyond int64 saturates rather than erroring: Python ints are
// unbounded, so the Python accepts such a value, and every consumer here either
// range-checks it afterwards (a port) or compares it against a byte counter that
// can never reach the saturation point (a budget). Saturating keeps the verdict
// identical where a hard error would diverge on a config the Python accepted.
func asPolicyInt(value any, where string) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, NewPolicyError("%s: expected an integer, got %s", where, policyRepr(value))
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			if strings.HasPrefix(number.String(), "-") {
				return math.MinInt, nil
			}
			return math.MaxInt, nil
		}
		return 0, NewPolicyError("%s: expected an integer, got %s", where, policyRepr(value))
	}
	return int(parsed), nil
}

// parsePolicyPort validates one port number.
func parsePolicyPort(value any, where string, profile string) (int, error) {
	port, err := asPolicyInt(value, where)
	if err != nil {
		return 0, err
	}
	if port < 1 || port > 65535 {
		return 0, NewPolicyError("%s: port %d out of range for profile '%s'", where, port, profile)
	}
	return port, nil
}

// parsePolicyIP resolves a host that may be an IP literal: surrounding
// whitespace and the [] brackets of a bracketed IPv6 literal are stripped first,
// and anything that is not an address is not an address (no resolution).
func parsePolicyIP(host string) (netip.Addr, bool) {
	candidate := strings.TrimSpace(host)
	if strings.HasPrefix(candidate, "[") && strings.HasSuffix(candidate, "]") {
		candidate = candidate[1 : len(candidate)-1]
	}
	address, err := netip.ParseAddr(candidate)
	if err != nil {
		return netip.Addr{}, false
	}
	return address, true
}

// sortedObjectKeys is a deterministic iteration order for the profile map, so a
// file with several bad profiles always reports the same one first.
func sortedObjectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// policyRepr renders a decoded JSON value the way Python's repr does, so the
// PolicyError text matches the Python's (strings single-quoted, True/False/None
// spelled out).
func policyRepr(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return "'" + typed + "'"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

// policyReprList renders a string slice as a Python list repr.
func policyReprList(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = "'" + value + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// Identity is a verified session identity (post token verification).
type Identity struct {
	SessionHash string
	Profile     string
}

// Decision is the enforce point's verdict: the kind/HTTP status plus the reason
// and the session's strike count that produced it.
type Decision struct {
	Kind    string
	Reason  string
	Strikes int
	Status  int
}

// sessionState is the in-memory per-session accounting: the timestamps of the
// denials inside the strike window and the advisory byte counter.
type sessionState struct {
	denies    []float64
	bytesUsed int
	lastSeen  float64
}

// EgressEngineConfig carries the engine knobs. A zero value selects the Python
// defaults (3 / 60s / 4096 / the wall clock), so a struct with the fields the
// caller cares about is always valid.
type EgressEngineConfig struct {
	// StrikeThreshold is EGRESS_STRIKE_THRESHOLD; 0 selects
	// DefaultStrikeThreshold.
	StrikeThreshold int
	// StrikeWindowS is EGRESS_STRIKE_WINDOW_S; 0 selects DefaultStrikeWindowS.
	StrikeWindowS float64
	// MaxSessions is EGRESS_MAX_SESSIONS; 0 selects DefaultMaxSessions.
	MaxSessions int
	// Clock is the time source (tests inject one); nil selects NowSeconds.
	Clock func() float64
}

// EgressEngine is the stateless policy plus in-memory per-session strike/byte
// accounting. The zero value is not usable: build it with NewEgressEngine.
type EgressEngine struct {
	Policy          *Policy
	StrikeThreshold int
	StrikeWindowS   float64
	MaxSessions     int

	clock    func() float64
	mu       sync.Mutex
	sessions map[string]*sessionState
	// order is the insertion order of sessions, so eviction picks the same
	// victim Python's min() over an insertion-ordered dict picks when several
	// sessions share a last_seen.
	order []string
}

// NewEgressEngine builds an engine over a loaded policy. A zero config field
// selects the Python default; a negative one is a programming error.
func NewEgressEngine(policy *Policy, cfg EgressEngineConfig) (*EgressEngine, error) {
	if cfg.StrikeThreshold < 0 {
		return nil, fmt.Errorf("strike_threshold must be >= 1, got %d", cfg.StrikeThreshold)
	}
	if cfg.StrikeWindowS < 0 {
		return nil, fmt.Errorf("strike_window_s must be positive, got %v", cfg.StrikeWindowS)
	}
	if cfg.MaxSessions < 0 {
		return nil, fmt.Errorf("max_sessions must be positive, got %d", cfg.MaxSessions)
	}
	threshold := cfg.StrikeThreshold
	if threshold == 0 {
		threshold = DefaultStrikeThreshold
	}
	window := cfg.StrikeWindowS
	if window == 0 {
		window = DefaultStrikeWindowS
	}
	maxSessions := cfg.MaxSessions
	if maxSessions == 0 {
		maxSessions = DefaultMaxSessions
	}
	clock := cfg.Clock
	if clock == nil {
		clock = NowSeconds
	}
	return &EgressEngine{
		Policy:          policy,
		StrikeThreshold: threshold,
		StrikeWindowS:   window,
		MaxSessions:     maxSessions,
		clock:           clock,
		sessions:        make(map[string]*sessionState),
	}, nil
}

// Evaluate decides host:port for identity using the engine clock.
func (e *EgressEngine) Evaluate(identity *Identity, host string, port int, byteHint int) Decision {
	return e.EvaluateAt(identity, host, port, byteHint, e.clock())
}

// EvaluateAt is Evaluate with an injected timestamp (the Python's now=).
func (e *EgressEngine) EvaluateAt(identity *Identity, host string, port int, byteHint int, now float64) Decision {
	if identity == nil {
		return Decision{Kind: Deny, Reason: ReasonUnauthenticated, Strikes: 0, Status: 403}
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	state := e.state(identity.SessionHash, now)
	if reason, killed := e.Policy.KillReason(host); killed {
		return Decision{Kind: Kill, Reason: reason, Strikes: e.noteDeny(state, now), Status: 403}
	}
	profile, ok := e.Policy.Profiles[identity.Profile]
	if !ok {
		return Decision{Kind: Deny, Reason: ReasonProfileUnknown, Strikes: e.window(state, now), Status: 403}
	}
	if !profile.Allows(host, port) {
		return e.deny(state, now, ReasonNotAllowlisted)
	}
	if profile.BudgetBytes != nil && byteHint > 0 {
		state.bytesUsed += byteHint
		if state.bytesUsed > *profile.BudgetBytes {
			return e.deny(state, now, ReasonBudgetExhausted)
		}
	}
	return Decision{Kind: Allow, Reason: ReasonAllowed, Strikes: e.window(state, now), Status: 200}
}

// SessionsSeen reports how many sessions the table holds (diagnostics).
func (e *EgressEngine) SessionsSeen() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sessions)
}

// deny records a strike and escalates to a kill once the threshold is reached.
func (e *EgressEngine) deny(state *sessionState, moment float64, reason string) Decision {
	strikes := e.noteDeny(state, moment)
	if strikes >= e.StrikeThreshold {
		return Decision{Kind: Kill, Reason: ReasonStrikes, Strikes: strikes, Status: 403}
	}
	return Decision{Kind: Deny, Reason: reason, Strikes: strikes, Status: 403}
}

// window drops the strikes that fell out of the trailing window and reports how
// many remain.
func (e *EgressEngine) window(state *sessionState, moment float64) int {
	cutoff := moment - e.StrikeWindowS
	drop := 0
	for drop < len(state.denies) && state.denies[drop] < cutoff {
		drop++
	}
	if drop > 0 {
		state.denies = append(state.denies[:0], state.denies[drop:]...)
	}
	return len(state.denies)
}

// noteDeny appends the current denial to the window and returns the new count.
func (e *EgressEngine) noteDeny(state *sessionState, moment float64) int {
	e.window(state, moment)
	state.denies = append(state.denies, moment)
	return len(state.denies)
}

// state returns the session's accounting record, creating it on first use and
// evicting the least recently used session when the table is full.
func (e *EgressEngine) state(sessionHash string, moment float64) *sessionState {
	state, ok := e.sessions[sessionHash]
	if !ok {
		if len(e.sessions) >= e.MaxSessions {
			e.evictOldest()
		}
		state = &sessionState{}
		e.sessions[sessionHash] = state
		e.order = append(e.order, sessionHash)
	}
	state.lastSeen = moment
	return state
}

// evictOldest drops the session with the oldest last_seen (ties go to the
// earliest inserted, mirroring the Python's min() over a dict).
func (e *EgressEngine) evictOldest() {
	victim := ""
	oldest := 0.0
	found := false
	for _, sessionHash := range e.order {
		state, ok := e.sessions[sessionHash]
		if !ok {
			continue
		}
		if !found || state.lastSeen < oldest {
			victim, oldest, found = sessionHash, state.lastSeen, true
		}
	}
	if !found {
		return
	}
	delete(e.sessions, victim)
	for index, sessionHash := range e.order {
		if sessionHash == victim {
			e.order = append(e.order[:index], e.order[index+1:]...)
			break
		}
	}
}