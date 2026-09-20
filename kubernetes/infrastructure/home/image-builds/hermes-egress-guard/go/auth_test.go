package guard

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Reference vectors computed with the Python implementation
// (src/egress_guard/auth.py) using secret b"unit-test-secret", session "a"*64,
// profile "python", expiry 1700000300 and the event body below — the Go port
// must reproduce them byte-for-byte, since the same wire constants gate the
// Hermes gateway and the live authorizer.
const (
	testSecret  = "unit-test-secret"
	testSession = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testExpiry  = 1700000300
	testNow     = 1700000000.0

	pythonToken = "v1.1700000300.python.TB5RzhoIpQB81BedAvUqaup8fZRGUgRUdsj41nlsMes"
)

var testEventBody = []byte(`{"event_id":"deadbeefdeadbeef","ts":1700000000,"kind":"deny"}`)

const pythonEventSignature = "v1=ef4d840b7ad16fce7b8faade18842ab650cb4e79fd76e79e67bad41be42a67e8"

func TestIdentityRegexes(t *testing.T) {
	sessionValid := []string{"a", "abc", "A1._-", strings.Repeat("x", 128)}
	for _, value := range sessionValid {
		if !SessionHashIsValid(value) {
			t.Errorf("SessionHashIsValid(%q) = false, want true", value)
		}
	}
	sessionInvalid := []string{"", strings.Repeat("x", 129), "has space", "sla/sh", "plus+", "ünïcode"}
	for _, value := range sessionInvalid {
		if SessionHashIsValid(value) {
			t.Errorf("SessionHashIsValid(%q) = true, want false", value)
		}
	}

	profileValid := []string{"go", "offline", "a-b-9", strings.Repeat("z", 32)}
	for _, value := range profileValid {
		if !ProfileIsValid(value) {
			t.Errorf("ProfileIsValid(%q) = false, want true", value)
		}
	}
	profileInvalid := []string{"", "Go", "PYTHON", "a b", "python_", "python!", strings.Repeat("z", 33)}
	for _, value := range profileInvalid {
		if ProfileIsValid(value) {
			t.Errorf("ProfileIsValid(%q) = true, want false", value)
		}
	}

	eventValid := []string{"deadbeef", "A1_-b2c3", strings.Repeat("d", 64)}
	for _, value := range eventValid {
		if !EventIDIsValid(value) {
			t.Errorf("EventIDIsValid(%q) = false, want true", value)
		}
	}
	eventInvalid := []string{"", "short7", strings.Repeat("d", 65), "has.dot", "has space"}
	for _, value := range eventInvalid {
		if EventIDIsValid(value) {
			t.Errorf("EventIDIsValid(%q) = true, want false", value)
		}
	}
}

func TestB64UrlAlphabetAndNopad(t *testing.T) {
	// 0xFB 0xFF encodes to "-_8" in the urlsafe alphabet; the standard alphabet
	// would emit "+/8", so this pins the alphabet choice.
	if got := B64UrlNopad([]byte{0xfb, 0xff}); got != "-_8" {
		t.Fatalf("B64UrlNopad(urlsafe bytes) = %q, want %q", got, "-_8")
	}
	// 22 bytes is 1 mod 3, so the padded form would carry "==" — the nopad form
	// must strip it.
	nopad := B64UrlNopad([]byte("any carnal pleasure."))
	if strings.Contains(nopad, "=") {
		t.Fatalf("B64UrlNopad(%q) emitted padding: %q", "any carnal pleasure.", nopad)
	}
	if got, want := nopad, "YW55IGNhcm5hbCBwbGVhc3VyZS4"; got != want {
		t.Fatalf("B64UrlNopad = %q, want %q", got, want)
	}
}

func TestB64UrlDecodePadsToBlock(t *testing.T) {
	// "-_8" is 3 chars; the helper must pad to "-_8=" itself.
	decoded, err := B64UrlDecode("-_8")
	if err != nil {
		t.Fatalf("B64UrlDecode(nopad input) error: %v", err)
	}
	if string(decoded) != string([]byte{0xfb, 0xff}) {
		t.Fatalf("B64UrlDecode = %x, want fbff", decoded)
	}
	if _, err := B64UrlDecode("- _8"); err == nil {
		t.Fatalf("B64UrlDecode accepted an invalid character")
	}

	original := []byte{0x00, 0x01, 0xfb, 0xff, 0x7f, 0x80}
	round, err := B64UrlDecode(B64UrlNopad(original))
	if err != nil {
		t.Fatalf("B64UrlDecode round trip error: %v", err)
	}
	if string(round) != string(original) {
		t.Fatalf("round trip = %x, want %x", round, original)
	}
}

func TestTokenMacInputLayout(t *testing.T) {
	got := string(TokenMacInput(testSession, testExpiry, "python"))
	want := "egress-token-v1|" + testSession + "|1700000300|python"
	if got != want {
		t.Fatalf("TokenMacInput = %q, want %q", got, want)
	}
}

func TestMintTokenMatchesPythonVector(t *testing.T) {
	token, err := MintToken([]byte(testSecret), testSession, "python", testExpiry)
	if err != nil {
		t.Fatalf("MintToken error: %v", err)
	}
	if token != pythonToken {
		t.Fatalf("MintToken = %q, want %q", token, pythonToken)
	}
	if !strings.HasPrefix(token, "v1."+strconv.Itoa(testExpiry)+".python.") {
		t.Fatalf("token layout = %q, want v1.<expiry>.<profile>.<mac>", token)
	}
	if parts := strings.Split(token, "."); len(parts) != 4 {
		t.Fatalf("token has %d dot-parts, want 4: %q", len(parts), token)
	}
}

func TestMintTokenRejectsBadIdentity(t *testing.T) {
	if _, err := MintToken([]byte(testSecret), "bad session", "python", testExpiry); err == nil {
		t.Fatalf("MintToken accepted an invalid session hash")
	}
	if _, err := MintToken([]byte(testSecret), testSession, "RUBY", testExpiry); err == nil {
		t.Fatalf("MintToken accepted an invalid profile")
	}
}

func TestVerifyTokenAcceptsFreshToken(t *testing.T) {
	check := VerifyToken([]byte(testSecret), testSession, pythonToken, testNow, DefaultTokenSkewS, DefaultTokenMaxTTLS)
	if !check.Ok || check.Reason != "ok" {
		t.Fatalf("VerifyToken = %+v, want ok", check)
	}
	if check.SessionHash != testSession || check.Profile != "python" || check.Expiry != testExpiry {
		t.Fatalf("VerifyToken identity = %+v", check)
	}
}

func TestVerifyTokenRejectsExpiredAndFuture(t *testing.T) {
	// expiry = 1700000300; now = expiry + 600 is past the 60s skew.
	expired := VerifyToken([]byte(testSecret), testSession, pythonToken, float64(testExpiry)+600, DefaultTokenSkewS, DefaultTokenMaxTTLS)
	if expired.Ok || expired.Reason != TokenExpired {
		t.Fatalf("VerifyToken(expired) = %+v, want %s", expired, TokenExpired)
	}
	// now = expiry - 86400 - 1 exceeds the default max TTL looking forward.
	future := VerifyToken([]byte(testSecret), testSession, pythonToken, float64(testExpiry)-DefaultTokenMaxTTLS-1, DefaultTokenSkewS, DefaultTokenMaxTTLS)
	if future.Ok || future.Reason != TokenNotYetValid {
		t.Fatalf("VerifyToken(future) = %+v, want %s", future, TokenNotYetValid)
	}
}

func TestVerifyTokenWindowBoundaries(t *testing.T) {
	expiry := float64(testExpiry)
	// Exactly at now+skew and now-max_ttl: both inside (inclusive bounds).
	if check := VerifyToken([]byte(testSecret), testSession, pythonToken, expiry+DefaultTokenSkewS, DefaultTokenSkewS, DefaultTokenMaxTTLS); !check.Ok {
		t.Fatalf("expiry == now+skew rejected: %+v", check)
	}
	if check := VerifyToken([]byte(testSecret), testSession, pythonToken, expiry-DefaultTokenMaxTTLS, DefaultTokenSkewS, DefaultTokenMaxTTLS); !check.Ok {
		t.Fatalf("expiry == now+max_ttl rejected: %+v", check)
	}
}

func TestVerifyTokenRejectsTamperedCredentials(t *testing.T) {
	mac := pythonToken[len("v1.1700000300.python."):]
	cases := []struct {
		name     string
		session  string
		password string
	}{
		{"bad mac", testSession, "v1.1700000300.python.AAAA"},
		{"other session", strings.Repeat("b", 64), pythonToken},
		{"not a token", testSession, "not-a-token"},
		{"three parts", testSession, "v1.1700000300.python"},
		{"five parts", testSession, "v1.1700000300.python." + mac + ".extra"},
		{"wrong prefix", testSession, "v2.1700000300.python." + mac},
		{"non digit expiry", testSession, "v1.17e9.python." + mac},
		{"empty mac", testSession, "v1.1700000300.python."},
		{"uppercase profile", testSession, "v1.1700000300.PYTHON." + mac},
		{"empty session hash", "", pythonToken},
		{"invalid session", "bad session", pythonToken},
	}
	for _, tc := range cases {
		check := VerifyToken([]byte(testSecret), tc.session, tc.password, testNow, DefaultTokenSkewS, DefaultTokenMaxTTLS)
		if check.Ok || check.Reason != TokenInvalid {
			t.Errorf("%s: VerifyToken = %+v, want reason %s", tc.name, check, TokenInvalid)
		}
	}
	// A token minted with a different secret must never verify.
	other, err := MintToken([]byte("another-secret"), testSession, "python", testExpiry)
	if err != nil {
		t.Fatalf("MintToken error: %v", err)
	}
	if check := VerifyToken([]byte(testSecret), testSession, other, testNow, DefaultTokenSkewS, DefaultTokenMaxTTLS); check.Ok {
		t.Fatalf("token from a foreign secret verified: %+v", check)
	}
}

func TestSignEventMatchesPythonVector(t *testing.T) {
	if got := SignEvent([]byte(testSecret), testEventBody); got != pythonEventSignature {
		t.Fatalf("SignEvent = %q, want %q", got, pythonEventSignature)
	}
}

func TestVerifyEventAcceptsOwnSignatureAndRejectsEverythingElse(t *testing.T) {
	secret := []byte(testSecret)
	if !VerifyEvent(secret, testEventBody, pythonEventSignature) {
		t.Fatalf("VerifyEvent rejected its own signature")
	}
	rejected := []struct {
		name      string
		secret    []byte
		body      []byte
		signature string
	}{
		{"empty signature", secret, testEventBody, ""},
		{"no prefix", secret, testEventBody, "deadbeef"},
		{"bare prefix", secret, testEventBody, "v1="},
		// The reaper signs the raw bytes it received, so a body mutation must
		// invalidate the signature; re-serialization cannot rescue it.
		{"mutated body", secret, []byte(strings.Replace(string(testEventBody), "deny", "kill", 1)), pythonEventSignature},
		{"foreign secret", []byte("another-secret"), testEventBody, pythonEventSignature},
		{"truncated digest", secret, testEventBody, pythonEventSignature[:len(pythonEventSignature)-2]},
		{"extended digest", secret, testEventBody, pythonEventSignature + "ab"},
	}
	for _, tc := range rejected {
		if VerifyEvent(tc.secret, tc.body, tc.signature) {
			t.Errorf("%s: VerifyEvent accepted an invalid signature", tc.name)
		}
	}
}

func TestReplayGuardAcceptsFreshEvent(t *testing.T) {
	guard := NewEventReplayGuard(DefaultReplayMaxEvents, DefaultReplaySkewS)
	if reason := guard.Check("deadbeefdeadbeef", json.Number("1700000000"), testNow); reason != nil {
		t.Fatalf("Check(fresh) = %q, want nil", *reason)
	}
}

func TestReplayGuardRejectsMalformedEvent(t *testing.T) {
	guard := NewEventReplayGuard(DefaultReplayMaxEvents, DefaultReplaySkewS)
	cases := []struct {
		name    string
		eventID string
		ts      any
		want    string
	}{
		{"short id", "abc", 1700000000, RejectionMalformedEventID},
		{"id with dot", "dead.beef.deadb", 1700000000, RejectionMalformedEventID},
		{"empty id", "", 1700000000, RejectionMalformedEventID},
		{"missing timestamp", "deadbeefdeadbeef", nil, RejectionMalformedTimestamp},
		{"string timestamp", "deadbeefdeadbeef", "1700000000", RejectionMalformedTimestamp},
		{"bool timestamp", "deadbeefdeadbeef", true, RejectionMalformedTimestamp},
		{"fractional timestamp", "deadbeefdeadbeef", 1700000000.5, RejectionMalformedTimestamp},
	}
	for _, tc := range cases {
		reason := guard.Check(tc.eventID, tc.ts, testNow)
		if reason == nil {
			t.Errorf("%s: Check accepted a malformed event", tc.name)
			continue
		}
		if *reason != tc.want {
			t.Errorf("%s: Check = %q, want %q", tc.name, *reason, tc.want)
		}
	}
}

func TestReplayGuardAcceptsEveryIntegralTimestampKind(t *testing.T) {
	guard := NewEventReplayGuard(DefaultReplayMaxEvents, DefaultReplaySkewS)
	// The decoded JSON shape depends on the caller's target type; every integral
	// representation of the same instant must be accepted.
	for _, ts := range []any{1700000000, int64(1700000000), float64(1700000000), json.Number("1700000000")} {
		if reason := guard.Check("deadbeefdeadbeef", ts, testNow); reason != nil {
			t.Errorf("Check(ts=%T) = %q, want nil", ts, *reason)
		}
	}
}

func TestReplayGuardRejectsStaleTimestamp(t *testing.T) {
	guard := NewEventReplayGuard(DefaultReplayMaxEvents, DefaultReplaySkewS)
	old := int64(testNow - DefaultReplaySkewS - 1)
	future := int64(testNow + DefaultReplaySkewS + 1)
	for _, ts := range []int64{old, future} {
		reason := guard.Check("deadbeefdeadbeef", ts, testNow)
		if reason == nil || *reason != RejectionStaleTimestamp {
			t.Fatalf("Check(ts=%d) = %v, want %q", ts, reason, RejectionStaleTimestamp)
		}
	}
	// Exactly on the boundary stays inside the window.
	if reason := guard.Check("deadbeefdeadbeef", int64(testNow-DefaultReplaySkewS), testNow); reason != nil {
		t.Fatalf("Check(at boundary) = %q, want nil", *reason)
	}
}

func TestReplayGuardRejectsReplay(t *testing.T) {
	guard := NewEventReplayGuard(DefaultReplayMaxEvents, DefaultReplaySkewS)
	if reason := guard.Check("deadbeefdeadbeef", 1700000000, testNow); reason != nil {
		t.Fatalf("Check(fresh) = %q, want nil", *reason)
	}
	guard.Record("deadbeefdeadbeef")
	reason := guard.Check("deadbeefdeadbeef", 1700000000, testNow)
	if reason == nil || *reason != RejectionReplay {
		t.Fatalf("Check(after record) = %v, want %q", reason, RejectionReplay)
	}
	// The guard only reports; an id the caller never recorded (a Kubernetes
	// failure path) stays replayable.
	if reason := guard.Check("cafebabecafebabe", 1700000000, testNow); reason != nil {
		t.Fatalf("Check(unrecorded peer) = %q, want nil", *reason)
	}
}

func TestReplayGuardEvictsOldestBeyondCapacity(t *testing.T) {
	guard := NewEventReplayGuard(3, DefaultReplaySkewS)
	for _, id := range []string{"event0001", "event0002", "event0003", "event0004"} {
		guard.Record(id)
	}
	if guard.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", guard.Len())
	}
	// event0001 was evicted first, so it is no longer a replay.
	if reason := guard.Check("event0001", 1700000000, testNow); reason != nil {
		t.Fatalf("Check(evicted id) = %q, want nil", *reason)
	}
	// The three newest survive.
	for _, id := range []string{"event0002", "event0003", "event0004"} {
		reason := guard.Check(id, 1700000000, testNow)
		if reason == nil || *reason != RejectionReplay {
			t.Fatalf("Check(%s) = %v, want %q", id, reason, RejectionReplay)
		}
	}
}

func TestReplayGuardReRecordKeepsIdNewest(t *testing.T) {
	guard := NewEventReplayGuard(3, DefaultReplaySkewS)
	for _, id := range []string{"event0001", "event0002", "event0003"} {
		guard.Record(id)
	}
	// Touching an existing id makes it the newest, so the next insert evicts
	// event0002 rather than event0001.
	guard.Record("event0001")
	guard.Record("event0004")
	if guard.Len() != 3 {
		t.Fatalf("Len() = %d, want 3 (re-record must not grow the LRU)", guard.Len())
	}
	if reason := guard.Check("event0001", 1700000000, testNow); reason == nil || *reason != RejectionReplay {
		t.Fatalf("re-recorded id was evicted: %v", reason)
	}
	if reason := guard.Check("event0002", 1700000000, testNow); reason != nil {
		t.Fatalf("Check(evicted event0002) = %q, want nil", *reason)
	}
}

func TestReplayGuardFillsToCapacityWithoutEviction(t *testing.T) {
	guard := NewEventReplayGuard(2, DefaultReplaySkewS)
	guard.Record("event0001")
	guard.Record("event0002")
	if guard.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", guard.Len())
	}
	for _, id := range []string{"event0001", "event0002"} {
		if reason := guard.Check(id, 1700000000, testNow); reason == nil || *reason != RejectionReplay {
			t.Fatalf("Check(%s) = %v, want %q", id, reason, RejectionReplay)
		}
	}
}

func TestReplayGuardDefaults(t *testing.T) {
	guard := NewEventReplayGuard(0, 0)
	if guard.MaxEvents != DefaultReplayMaxEvents || guard.SkewS != DefaultReplaySkewS {
		t.Fatalf("NewEventReplayGuard(0,0) = %d/%v, want the Python defaults", guard.MaxEvents, guard.SkewS)
	}
}
