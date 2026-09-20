// HMAC identity and event signing (port of src/egress_guard/auth.py).
//
// TOKEN BYTE LAYOUT — authoritative, Hermes mints it, the authorizer verifies it
// -----------------------------------------------------------------------------
// The gateway injects HTTP_PROXY/HTTPS_PROXY with the session token as proxy
// userinfo:
//
//	http://<session-hash>:<token>@hermes-egress.hermes-egress.svc.cluster.local:3128
//
// Envoy turns that into a "Proxy-Authorization: Basic base64(<user>:<pass>)"
// header forwarded verbatim to ext_authz, so the authorizer sees exactly:
//
//	session_hash = the Basic username
//	token        = the Basic password
//
// and the password is:
//
//	"v1." <expiry_unix> "." <profile> "." <mac>
//	mac = b64url_nopad(HMAC_SHA256(secret,
//	          b"egress-token-v1|" + session_hash + b"|" + expiry_unix + b"|" + profile))
//
// with expiry_unix written in ASCII decimal. session_hash must match
// [A-Za-z0-9._-]{1,128} and profile [a-z0-9-]{1,32} (the fixed vocabulary:
// core, offline, python, go, node, web).
//
// Why the profile is inside the MAC (and not an x-egress-profile header): the
// sandbox controls its own HTTP headers, so anything but the verified token is
// untrusted, and the authorizer has no Kubernetes API access to look the claim's
// label up. Riding the MAC keeps the authorizer stateless about identity.
//
// Accepted window: now - EGRESS_TOKEN_SKEW_S (default 60s) through
// now + EGRESS_TOKEN_MAX_TTL_S (default 86400s). Tokens are not single-use:
// every proxied request re-presents the same proxy credentials, so replay
// protection applies to *events*, not to the session token.
//
// EVENTS — authorizer -> reaper
// -----------------------------
// Body (compact, sort_keys=true JSON, serialized once by the sender):
//
//	{"event_id": "<uuid4 hex>", "ts": <unix int>, "kind": "deny"|"kill",
//	 "session_hash": "<hash>"|null, "profile": ..., "target_host": ...,
//	 "target_port": ..., "reason": ..., "strikes": <int>, "source_ip": ...}
//
// Header "x-egress-signature: v1=<hex HMAC_SHA256(secret, raw body bytes)>".
// The reaper verifies over the raw bytes it received (no JSON re-serialization,
// no canonicalization ambiguity) and only then parses.
package guard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token prefix, MAC domain separator and event-signature prefix. These are wire
// constants: changing any of them invalidates every minted credential.
const (
	// TokenPrefix is the version tag of the proxy password ("v1.<expiry>.<profile>.<mac>").
	TokenPrefix = "v1"
	// SignaturePrefix prefixes the event HMAC hex digest in the x-egress-signature header.
	SignaturePrefix = "v1="
)

// TokenMacDomain is the domain separator bound into every token MAC. It keeps a
// token MAC from ever equalling an event-signature-style digest over the same
// suffix bytes.
var TokenMacDomain = []byte("egress-token-v1")

// Reasons the authorizer reports in x-egress-reason / events for
// pre-authentication failures. None of them counts a strike.
const (
	TokenMissing     = "token-missing"
	TokenInvalid     = "token-invalid"
	TokenExpired     = "token-expired"
	TokenNotYetValid = "token-not-yet-valid"
)

// Defaults mirroring the Python keyword defaults (EGRESS_TOKEN_SKEW_S /
// EGRESS_TOKEN_MAX_TTL_S at the call sites).
const (
	// DefaultTokenSkewS tolerates a little clock drift between minter and verifier.
	DefaultTokenSkewS = 60.0
	// DefaultTokenMaxTTLS bounds how far in the future a token may be minted.
	DefaultTokenMaxTTLS = 86400.0
	// DefaultReplayMaxEvents is the seen-id LRU capacity of EventReplayGuard.
	DefaultReplayMaxEvents = 4096
	// DefaultReplaySkewS is the accepted |now-ts| window of EventReplayGuard.
	DefaultReplaySkewS = 300.0
)

// Rejection reasons returned by EventReplayGuard.Check.
const (
	// RejectionMalformedEventID: the id is absent or outside EVENT_ID_RE.
	RejectionMalformedEventID = "malformed-event-id"
	// RejectionMalformedTimestamp: ts is absent, a bool, or not an integer.
	RejectionMalformedTimestamp = "malformed-timestamp"
	// RejectionStaleTimestamp: |now - ts| exceeds the skew window.
	RejectionStaleTimestamp = "stale-timestamp"
	// RejectionReplay: the id was already successfully processed.
	RejectionReplay = "replay"
)

// Identity grammar, taken literally from the Python module.
//
// Go's `$` anchors at end-of-text where Python's also matches just before a
// trailing newline; Go is therefore *stricter* here and rejects "abc\n" that
// Python's re.match would accept. That deviation only ever turns an accepted
// credential into a rejected one, never the other way around.
var (
	// sessionHashRe mirrors SESSION_HASH_RE.
	sessionHashRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	// profileRe mirrors PROFILE_RE.
	profileRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	// eventIDRe mirrors EVENT_ID_RE.
	eventIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
)

// b64urlNopadEncoding is the urlsafe alphabet minus padding: there is no
// base64.URLEncoding constant for the nopad form, so the alphabet is spelled out
// and "=" stripped after encoding.
var b64urlNopadEncoding = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")

// SessionHashIsValid reports whether value matches [A-Za-z0-9._-]{1,128}.
func SessionHashIsValid(value string) bool { return sessionHashRe.MatchString(value) }

// ProfileIsValid reports whether value matches [a-z0-9-]{1,32}.
func ProfileIsValid(value string) bool { return profileRe.MatchString(value) }

// EventIDIsValid reports whether value matches [A-Za-z0-9_-]{8,64}.
func EventIDIsValid(value string) bool { return eventIDRe.MatchString(value) }

// B64UrlNopad encodes data with the urlsafe alphabet and no "=" padding,
// mirroring Python's b64url_nopad.
func B64UrlNopad(data []byte) string {
	return strings.ReplaceAll(b64urlNopadEncoding.EncodeToString(data), "=", "")
}

// B64UrlDecode pads text to a multiple of four and decodes it as urlsafe base64,
// mirroring Python's b64url_decode. Malformed input returns an error where
// Python's decoder would silently drop stray characters; every producer in this
// repo emits valid nopad base64, so the strictness only surfaces on tampering.
func B64UrlDecode(text string) ([]byte, error) {
	// Python's (-len(text) % 4) is a true modulus; Go's % keeps the dividend's
	// sign, so "-3 % 4" is -3 here and would panic in strings.Repeat. Normalize.
	padding := (4 - len(text)%4) % 4
	return b64urlNopadEncoding.DecodeString(text + strings.Repeat("=", padding))
}

// TokenMacInput builds the domain-separated message the token MAC covers:
// "egress-token-v1|" + session_hash + "|" + expiry + "|" + profile.
func TokenMacInput(sessionHash string, expiry int, profile string) []byte {
	parts := []byte{}
	parts = append(parts, TokenMacDomain...)
	parts = append(parts, '|')
	parts = append(parts, sessionHash...)
	parts = append(parts, '|')
	parts = append(parts, strconv.Itoa(expiry)...)
	parts = append(parts, '|')
	parts = append(parts, profile...)
	return parts
}

// TokenMac returns b64url_nopad(HMAC_SHA256(secret, TokenMacInput(...))).
func TokenMac(secret []byte, sessionHash string, expiry int, profile string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(TokenMacInput(sessionHash, expiry, profile))
	return B64UrlNopad(mac.Sum(nil))
}

// MintToken builds the proxy password "v1.<expiry>.<profile>.<mac>". Both this
// minter and VerifyToken must agree byte-for-byte with the layout in the file
// header.
func MintToken(secret []byte, sessionHash string, profile string, expiry int) (string, error) {
	if !SessionHashIsValid(sessionHash) {
		return "", fmt.Errorf("invalid session hash: %q", sessionHash)
	}
	if !ProfileIsValid(profile) {
		return "", fmt.Errorf("invalid profile: %q", profile)
	}
	mac := TokenMac(secret, sessionHash, expiry, profile)
	return TokenPrefix + "." + strconv.Itoa(expiry) + "." + profile + "." + mac, nil
}

// TokenCheck is the outcome of VerifyToken. Mirrors the Python frozen
// dataclass: Ok with a Reason, plus the verified identity when Ok.
type TokenCheck struct {
	Ok          bool
	Reason      string
	SessionHash string
	Profile     string
	Expiry      int
}

// VerifyToken validates a proxy password against the session hash it was minted
// for, the clock, and the accepted [now-skew, now+max_ttl] window.
func VerifyToken(secret []byte, sessionHash string, password string, now float64, skewS float64, maxTTLS float64) TokenCheck {
	if !SessionHashIsValid(sessionHash) {
		return TokenCheck{Reason: TokenInvalid}
	}
	parts := strings.Split(password, ".")
	if len(parts) != 4 || parts[0] != TokenPrefix {
		return TokenCheck{Reason: TokenInvalid}
	}
	rawExpiry, profile, mac := parts[1], parts[2], parts[3]
	if !isASCIIDigits(rawExpiry) || !ProfileIsValid(profile) || mac == "" {
		return TokenCheck{Reason: TokenInvalid}
	}
	// strconv.ParseInt would accept a leading sign; the digits check above (the
	// Python .isdigit()) already rejected one, so this cannot fail — but an
	// over-long digit run must not silently wrap.
	expiry64, err := strconv.ParseInt(rawExpiry, 10, 64)
	if err != nil {
		return TokenCheck{Reason: TokenInvalid}
	}
	expiry := int(expiry64)
	if !hmac.Equal([]byte(TokenMac(secret, sessionHash, expiry, profile)), []byte(mac)) {
		return TokenCheck{Reason: TokenInvalid}
	}
	if float64(expiry) < now-skewS {
		return TokenCheck{Reason: TokenExpired}
	}
	if float64(expiry) > now+maxTTLS {
		return TokenCheck{Reason: TokenNotYetValid}
	}
	return TokenCheck{Ok: true, Reason: "ok", SessionHash: sessionHash, Profile: profile, Expiry: expiry}
}

// isASCIIDigits mirrors Python's str.isdigit() for the ASCII decimal digits a
// proxy password can carry. Python's isdigit() is true for some non-ASCII digit
// runes (superscripts, other scripts); those can never come off this wire (the
// token is split on "." from a Basic-password header), so the ASCII check is the
// same predicate in practice and stricter if it ever is not. An empty run is not
// a digit run.
func isASCIIDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// SignEvent returns "v1=" + hex(HMAC_SHA256(secret, body)) over the raw body
// bytes, exactly as the sender serialized them.
func SignEvent(secret []byte, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// VerifyEvent re-computes the event signature over the raw received bytes and
// compares it in constant time. A missing signature (empty string, the Go
// equivalent of Python's None header) is a failure.
func VerifyEvent(secret []byte, body []byte, signature string) bool {
	if !strings.HasPrefix(signature, SignaturePrefix) {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature[len(SignaturePrefix):]))
}

// EventReplayGuard is the timestamp window plus seen-id LRU that keeps a
// re-delivered event from being processed twice.
//
// Check only reports; the caller calls Record *after* the event was processed
// successfully, so a transient Kubernetes failure does not burn the event id and
// a retry still works.
//
// Python's OrderedDict is reached from multiple request threads; here a mutex
// guards the map and its FIFO order slice, and the LRU touch is a
// remove-then-append (O(capacity) only on the rare already-seen path).
type EventReplayGuard struct {
	MaxEvents int
	SkewS     float64

	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

// NewEventReplayGuard builds a guard. A non-positive maxEvents or skewS selects
// the Python defaults, so a zero-valued config field cannot silently disable
// replay protection or shrink the window to nothing.
func NewEventReplayGuard(maxEvents int, skewS float64) *EventReplayGuard {
	if maxEvents <= 0 {
		maxEvents = DefaultReplayMaxEvents
	}
	if skewS <= 0 {
		skewS = DefaultReplaySkewS
	}
	return &EventReplayGuard{
		MaxEvents: maxEvents,
		SkewS:     skewS,
		seen:      make(map[string]struct{}, maxEvents),
		order:     make([]string, 0, maxEvents),
	}
}

// Check reports whether the event may be processed, returning nil when it may,
// otherwise one of the Rejection* reasons.
//
// ts is accepted as any because the decoded JSON value reaches this function
// through whatever the caller decoded into: encoding/json yields float64 for an
// untyped number, a typed event struct yields an int kind, and json.Number is
// also possible. Integral values of every numeric kind are accepted (the wire
// value *was* an integer), while bools, strings, nil and fractional numbers are
// malformed — matching the Python isinstance(ts, int) with its bool exclusion.
func (g *EventReplayGuard) Check(eventID string, ts any, now float64) *string {
	if !EventIDIsValid(eventID) {
		reason := RejectionMalformedEventID
		return &reason
	}
	seconds, ok := integerSeconds(ts)
	if !ok {
		reason := RejectionMalformedTimestamp
		return &reason
	}
	if math.Abs(now-float64(seconds)) > g.SkewS {
		reason := RejectionStaleTimestamp
		return &reason
	}
	g.mu.Lock()
	_, seen := g.seen[eventID]
	g.mu.Unlock()
	if seen {
		reason := RejectionReplay
		return &reason
	}
	return nil
}

// Record marks an event id as processed and evicts the oldest ids beyond the
// capacity, mirroring OrderedDict.move_to_end + popitem(last=False).
func (g *EventReplayGuard) Record(eventID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.seen[eventID]; exists {
		for i, id := range g.order {
			if id == eventID {
				g.order = append(g.order[:i], g.order[i+1:]...)
				break
			}
		}
	}
	g.seen[eventID] = struct{}{}
	g.order = append(g.order, eventID)
	for len(g.order) > g.MaxEvents {
		evicted := g.order[0]
		g.order = g.order[1:]
		delete(g.seen, evicted)
	}
}

// Len reports how many ids are currently retained (test/observability helper).
func (g *EventReplayGuard) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.order)
}

// integerSeconds coerces a decoded JSON timestamp to integer seconds.
func integerSeconds(ts any) (int64, bool) {
	switch value := ts.(type) {
	case nil:
		return 0, false
	case bool:
		// Python rejects bool even though it is an int subclass.
		return 0, false
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		return int64(value), true
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		if value > math.MaxInt64 {
			return 0, false
		}
		return int64(value), true
	case float32:
		return integralFloat(float64(value))
	case float64:
		return integralFloat(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// integralFloat accepts only whole numbers: a JSON float timestamp is not an int
// in Python either.
func integralFloat(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) {
		return 0, false
	}
	if value < math.MinInt64 || value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

// NowSeconds is the wall clock as fractional Unix seconds.
func NowSeconds() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}
