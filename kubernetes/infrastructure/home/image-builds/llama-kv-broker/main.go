// Command llama-kv-broker gives the qwen36-35b llama.cpp server persistent,
// per-session KV caches.
//
// WHY THIS EXISTS — llama.cpp keeps the KV cache in process memory only, so every
// new conversation (and every serving-pod restart) re-prefills the whole prompt.
// With --slot-save-path the server can dump that cache to disk; this broker is the
// only component allowed to talk to the slot endpoints, so it can run the
// restore -> infer -> save choreography and keep the raw /slots API unreachable
// from outside.
//
// WHY ONE GLOBAL MUTEX — the serving pod runs with --parallel 1, i.e. exactly one
// slot. Per-session locks therefore cannot protect the slot they all share, so
// every transactional request and every sweep serializes on one mutex; concurrent
// requests queue exactly like they already queue inside the single-slot server.
//
// WHY A CONFIG FINGERPRINT — a slot file is only a valid KV cache for the model and
// flags it was written with. Folding (image, model, ctx size, cache types,
// flash-attn, /props) into the file name means a changed serving spec simply stops
// matching the old files: never restored, left for the sweeper to reclaim, so the
// slot directory stays disposable.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultSlotPath   = "/var/lib/llama/kv"
	defaultListenAddr = ":8080"
	defaultPropsTTL   = 30 * time.Second
	// 14 days: a long conversation survives a two-week gap, an abandoned one still
	// leaves on its own.
	defaultTTL = 336 * time.Hour
	// 80 GiB of the 100 Gi slot PVC: leaves the PVC headroom for one in-flight save.
	defaultCapBytes = int64(85899345920)

	// Larger bodies are proxied untouched: they are exactly the ones a streaming
	// client would not want buffered anyway.
	maxTxnBodyBytes = 64 << 20
	slotOpTimeout   = 30 * time.Second
	// /props is tiny and only feeds a fingerprint, so it must never delay a
	// request for long — a miss just means this request runs without slots.
	propsTimeout = 5 * time.Second
	// The /slots probe reads the serving server's restart signal and runs under
	// the transaction mutex, so it is deliberately shorter than propsTimeout: a
	// hung probe must not park every queued request for five seconds.
	slotProbeTimeout = 2 * time.Second
	sweepEvery       = 24 * time.Hour

	slotPrefix  = "slot-"
	slotSuffix  = ".bin"
	slotHashLen = 20

	actionRestore = "restore"
	actionSave    = "save"
	actionErase   = "erase"

	// KV_RESTORE_MODE selects when a slot file may be restored into the live
	// slot: off (the shipped default) never restores, auto restores once per
	// file after a detected server restart, always restores whenever the file
	// exists.
	restoreModeOff    = "off"
	restoreModeAuto   = "auto"
	restoreModeAlways = "always"
)

// hopHeaders are connection-scoped and must not be forwarded in either direction.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func main() {
	cfg := loadConfig()
	if cfg.upstreamURL == "" {
		log.Fatal("UPSTREAM_URL is required (e.g. http://qwen36-35b.llmkube-system.svc.cluster.local:8080)")
	}
	if err := os.MkdirAll(cfg.slotPath, 0o755); err != nil {
		log.Fatalf("create slot dir %s: %v", cfg.slotPath, err)
	}
	b, err := newBroker(cfg)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("llama-kv-broker %s", cfg)
	// Startup sweep: a restarted broker is the only chance to reclaim what the
	// previous pod left behind, and it runs before the listener takes traffic.
	b.sweep()
	go b.sweepLoop()
	srv := &http.Server{
		Addr:    cfg.listenAddr,
		Handler: b,
		// Deliberately no read/write timeouts: one completion streams for minutes.
		// The slot calls carry their own 30s deadlines and the request header read
		// is the only part an unresponsive peer can stall cheaply.
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("listening on %s, upstream %s", cfg.listenAddr, cfg.upstreamURL)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// config is the broker's entire runtime tuning, all of it from the environment so
// the Deployment is the single source of truth.
type config struct {
	upstreamURL string
	slotPath    string
	listenAddr  string

	// The serving spec the slot files belong to; these are the env half of the
	// fingerprint.
	imageTag   string
	modelRef   string
	ctxSize    string
	cacheTypeK string
	cacheTypeV string
	flashAttn  string

	restoreMode string

	propsTTL time.Duration
	ttl      time.Duration
	capBytes int64
}

func (c config) String() string {
	return fmt.Sprintf(
		"upstream=%s slot_path=%s listen=%s restore_mode=%s fingerprint=[image=%q model=%q ctx=%q cache_k=%q cache_v=%q flash_attn=%q] props_ttl=%s slot_ttl=%s cap_bytes=%d",
		c.upstreamURL, c.slotPath, c.listenAddr, c.restoreMode, c.imageTag, c.modelRef,
		c.ctxSize, c.cacheTypeK, c.cacheTypeV, c.flashAttn, c.propsTTL, c.ttl, c.capBytes)
}

func loadConfig() config {
	// Strict on purpose: a typo in the Deployment must not quietly re-enable
	// the restore path, so anything outside the three values falls back to off
	// with a warning. Blank falls to the default without a warning.
	mode := envStr("KV_RESTORE_MODE", restoreModeOff)
	switch mode {
	case restoreModeOff, restoreModeAuto, restoreModeAlways:
	default:
		log.Printf("warn: KV_RESTORE_MODE=%q is not one of off|auto|always, restoring stays off", mode)
		mode = restoreModeOff
	}
	return config{
		upstreamURL: strings.TrimRight(envStr("UPSTREAM_URL", ""), "/"),
		slotPath:    envStr("SLOT_PATH", defaultSlotPath),
		listenAddr:  envStr("LISTEN_ADDR", defaultListenAddr),
		restoreMode: mode,
		imageTag:    envStr("KV_IMAGE_TAG", ""),
		modelRef:    envStr("KV_MODEL_REF", ""),
		ctxSize:     envStr("KV_CTX_SIZE", ""),
		cacheTypeK:  envStr("KV_CACHE_TYPE_K", ""),
		cacheTypeV:  envStr("KV_CACHE_TYPE_V", ""),
		flashAttn:   envStr("KV_FLASH_ATTN", ""),
		propsTTL:    envDuration("KV_PROPS_TTL", defaultPropsTTL),
		ttl:         envDuration("KV_TTL", defaultTTL),
		capBytes:    envBytes("KV_CAP_BYTES", defaultCapBytes),
	}
}

// envStr treats an unset or blank variable as absent, so an empty value in the
// Deployment falls back to the default instead of disabling the component.
func envStr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := envStr(key, "")
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warn: %s=%q is not a duration (%v), using %s", key, v, err, fallback)
		return fallback
	}
	return d
}

func envBytes(key string, fallback int64) int64 {
	v := envStr(key, "")
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("warn: %s=%q is not a byte count (%v), using %d", key, v, err, fallback)
		return fallback
	}
	return n
}

// broker owns every piece of shared state: the slot directory, the /props cache
// and the upstream clients.
type broker struct {
	cfg    config
	client *http.Client
	proxy  *httputil.ReverseProxy

	// mu covers everything that touches slot state — the restore->infer->save
	// transaction, the sweeper and slotForm. It is held across the whole
	// transaction on purpose: with a single slot, per-session locking would let
	// two conversations rewrite the same slot concurrently.
	mu sync.Mutex
	// slotForm remembers which slot endpoint shape the running server answers;
	// it is only read and written while mu is held.
	slotForm slotForm

	// The /props cache has its own lock so a slow fingerprint fetch never
	// serializes requests behind mu.
	propsMu    sync.Mutex
	propsAt    time.Time
	propsCache propsFields
	propsOK    bool

	// lastSlotTaskID is the serving server's own task counter for slot 0. It only
	// grows inside one server process, so a value lower than the previous
	// observation means the server restarted and its in-memory cache is gone;
	// that decrease — never the absolute value — is the restart signal.
	lastSlotTaskID int64
	// pendingRestore holds the slot files that owe exactly one restore: every file
	// that was on disk when a restart was detected. An empty set is the warm case,
	// where the live slot must be left alone. Guarded by mu.
	pendingRestore map[string]bool

	metrics *metrics
}

// slotForm selects between the documented `?action=` endpoint and the path-form
// variant that some llama.cpp builds expose instead.
type slotForm int

const (
	slotFormQuery slotForm = iota
	slotFormPath
)

func newBroker(cfg config) (*broker, error) {
	u, err := url.Parse(cfg.upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("UPSTREAM_URL %q: %w", cfg.upstreamURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("UPSTREAM_URL %q: needs a scheme and a host", cfg.upstreamURL)
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	// -1 flushes every write straight through: streaming is the whole point of
	// the passthrough paths.
	proxy.FlushInterval = -1
	proxy.ErrorLog = log.New(log.Writer(), "proxy: ", log.LstdFlags)
	m := newMetrics()
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		m.incUpstreamError("proxy")
		log.Printf("warn: proxy %s %s: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		// The broker is a correct client, so a non-2xx here is the upstream
		// failing to serve, not a request-shape problem.
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			m.incUpstreamError("proxy")
		}
		return nil
	}
	return &broker{
		cfg:            cfg,
		client:         &http.Client{Transport: transport},
		proxy:          proxy,
		pendingRestore: make(map[string]bool),
		metrics:        m,
	}, nil
}

// statusWriter records the status a request settles on, which is what labels its
// outcome in kv_broker_requests_total. Unwrap and Flush keep
// http.ResponseController — and with it streaming through the proxy and the
// transactional path — working exactly as it does on the raw writer.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// requestResult maps a settled HTTP status onto the counter label: ok means the
// caller got a 2xx, which is the only outcome that is not an error somewhere.
func requestResult(status int) string {
	if status >= 200 && status <= 299 {
		return "ok"
	}
	return "error"
}

// handlerLabel mirrors ServeHTTP's routing, so the counter labels the route the
// request actually took (a proxied /v1/* path is v1_other, not a completion, and a
// non-GET /healthz falls through to the 404 branch).
func handlerLabel(r *http.Request) string {
	switch {
	case r.URL.Path == "/metrics":
		return "metrics"
	case r.URL.Path == "/healthz":
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return "healthz"
		}
		return "not_found"
	case strings.HasPrefix(r.URL.Path, "/slots"):
		return "rejected"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		return "chat_completions"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/completions":
		return "completions"
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		return "v1_other"
	default:
		return "not_found"
	}
}

func (b *broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusWriter{ResponseWriter: w}
	w = rec
	defer func() { b.metrics.incRequest(handlerLabel(r), requestResult(rec.status)) }()

	switch {
	case r.URL.Path == "/metrics":
		// Local, like /healthz: the broker's own metrics never traverse the
		// upstream, and the upstream's /metrics stays unreachable through here.
		b.metrics.serveHTTP(w, r)
	case r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	case strings.HasPrefix(r.URL.Path, "/slots"):
		// The raw slot API stays broker-internal: a caller able to rewrite slot
		// state behind the transaction could poison another conversation's cache.
		http.Error(w, "forbidden", http.StatusForbidden)
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		if r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions") {
			b.transactional(w, r)
			return
		}
		// Everything else (notably GET /v1/models) is a plain streaming proxy.
		b.proxy.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// transactional runs the restore -> infer -> save choreography for one completion
// request. Every early return falls back to the plain proxy: persistence is an
// optimization, never a precondition for a correct answer.
func (b *broker) transactional(w http.ResponseWriter, r *http.Request) {
	b.metrics.incInflight()
	defer b.metrics.decInflight()

	body, ok := readCapped(r)
	if !ok {
		log.Printf("warn: %s body unreadable or over %d bytes, proxying without slots", r.URL.Path, maxTxnBodyBytes)
		b.proxy.ServeHTTP(w, r)
		return
	}
	parsed, ok := decodeBody(body)
	if !ok {
		log.Printf("warn: %s body is not a JSON object, proxying without slots", r.URL.Path)
		b.proxy.ServeHTTP(w, r)
		return
	}
	key, ok := sessionKey(r, parsed)
	if !ok {
		log.Printf("warn: %s carries no session identity (X-Session-Id, user, first user turn), proxying without slots", r.URL.Path)
		b.proxy.ServeHTTP(w, r)
		return
	}
	// Fingerprint before taking mu: a slow /props must not serialize requests that
	// could already be answered.
	fp, ok := b.fingerprint(r.Context())
	if !ok {
		log.Printf("warn: /props unreachable, proxying without slots")
		b.proxy.ServeHTTP(w, r)
		return
	}
	// cache_prompt makes the server reuse the restored prefix instead of
	// discarding it; safe on a cold slot too.
	parsed["cache_prompt"] = json.RawMessage("true")
	out, err := json.Marshal(parsed)
	if err != nil {
		log.Printf("warn: re-encode body: %v, proxying without slots", err)
		b.proxy.ServeHTTP(w, r)
		return
	}
	slot := slotFileName(key, fp)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Slot calls outlive the client: a disconnect must not leave a half-applied
	// erase or drop a save the server already produced state for.
	opCtx := context.WithoutCancel(r.Context())

	// The restore decision is mode-gated. off — the shipped default — runs no
	// probe and touches no slot endpoint: the live measurement (restore is a
	// destructive no-op on this build, 130 ms -> 44 s) is not worth paying on
	// the serving path while the KV-layout experiment (--kv-unified /
	// --ctx-checkpoints) that could make restores real stays pending. Saves
	// keep running either way, so flipping the mode later starts from real
	// slot files.
	slotAbs := filepath.Join(b.cfg.slotPath, slot)
	switch b.cfg.restoreMode {
	case restoreModeOff:
		// Nothing: no /slots probe, no restore, no erase. Saves below are the
		// only slot traffic.
	case restoreModeAuto:
		// Read the live slot's task counter before deciding to restore. Only a
		// server restart (its counter went backwards) makes a file restore
		// worth having; a probe failure changes no state and never blocks the
		// request.
		b.observeSlotTaskLocked(opCtx)
		if !b.pendingRestore[slot] {
			// The slot is warm: llama.cpp still holds this conversation's
			// prefix in memory, so a restore would be inert *and* destroy that
			// cache (measured on the P100: 130 ms -> 44 s of prompt
			// processing). Leave it alone.
			b.metrics.incRestore("skipped_warm")
		} else {
			b.restoreFromFileLocked(opCtx, slot, slotAbs)
		}
	case restoreModeAlways:
		if _, err := os.Stat(slotAbs); err == nil {
			b.restoreFromFileLocked(opCtx, slot, slotAbs)
		} else {
			// No file yet — nothing to restore over, and the live cache is the
			// only one there is.
			b.metrics.incRestore("skipped_no_file")
		}
	}

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, b.cfg.upstreamURL+r.URL.RequestURI(), bytes.NewReader(out))
	if err != nil {
		log.Printf("warn: build upstream request: %v", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	forwardRequestHeaders(upReq.Header, r.Header)
	upReq.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(upReq)
	if err != nil {
		b.metrics.incUpstreamError("chat")
		log.Printf("warn: upstream %s failed: %v", r.URL.Path, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	// Hand-rolled copy rather than ReverseProxy: only io.EOF tells us the whole
	// generation reached the client, and only then is the slot worth saving.
	stream := &flushingWriter{w: w, rc: http.NewResponseController(w)}
	_, copyErr := io.Copy(stream, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b.metrics.incUpstreamError("chat")
		b.metrics.incSave("skipped_status")
		return
	}
	if copyErr != nil {
		b.metrics.incSave("skipped_no_eof")
		log.Printf("warn: client went away before EOF, not saving %s: %v", slot, copyErr)
		return
	}
	saveStart := time.Now()
	saveErr := b.slotOp(opCtx, actionSave, slot)
	b.metrics.observeSave(time.Since(saveStart))
	if saveErr != nil {
		b.metrics.incSave("error")
		log.Printf("warn: save %s failed: %v", slot, saveErr)
		return
	}
	b.metrics.incSave("ok")
	log.Printf("save %s", slot)
}

// restoreFromFileLocked runs the one-shot restore obligation for a slot file
// that is known to exist: restore it into the live slot, and on any failure
// erase the live slot and unlink the file so the request serves cold from a
// clean slate. It exists so the auto (restart-marked) and always (file-exists)
// gates share one failure policy.
func (b *broker) restoreFromFileLocked(ctx context.Context, slot, slotAbs string) {
	// One shot: whichever way this goes, the obligation is discharged.
	delete(b.pendingRestore, slot)
	restoreStart := time.Now()
	err := b.slotOp(ctx, actionRestore, slot)
	b.metrics.observeRestore(time.Since(restoreStart))
	if err != nil {
		b.metrics.incRestore("error")
		log.Printf("warn: restore %s failed (%v), erasing the slot and serving cold", slot, err)
		if e := b.slotOp(ctx, actionErase, slot); e != nil {
			log.Printf("warn: erase %s failed: %v", slot, e)
		}
		if e := os.Remove(slotAbs); e != nil && !os.IsNotExist(e) {
			log.Printf("warn: remove %s failed: %v", slot, e)
		}
	} else {
		b.metrics.incRestore("ok")
		log.Printf("restore %s", slot)
	}
}

// replayBody serves a buffered prefix and then the untouched remainder of the
// original body, keeping the original's Close so the server still tears the
// request down exactly as it would have.
type replayBody struct {
	io.Reader
	closer io.Closer
}

func (b replayBody) Close() error { return b.closer.Close() }

// readCapped buffers the body and puts the stream back on r, so the fallback proxy
// can forward an oversized or unparseable body exactly as it arrived.
func readCapped(r *http.Request) ([]byte, bool) {
	origin := r.Body
	buf, err := io.ReadAll(io.LimitReader(origin, maxTxnBodyBytes+1))
	r.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(buf), origin), closer: origin}
	if err != nil || len(buf) > maxTxnBodyBytes {
		return nil, false
	}
	return buf, true
}

// decodeBody parses the request as a JSON object while leaving every nested value
// untouched (RawMessage), so re-encoding cannot disturb the payload beyond the
// fields this broker sets.
func decodeBody(body []byte) (map[string]json.RawMessage, bool) {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil || parsed == nil {
		return nil, false
	}
	return parsed, true
}

// sessionKey derives the conversation identity, preferring an explicit header over
// the request's end-user over the conversation's own opening turn. The prefixes
// keep the three sources from ever colliding inside the slot hash.
func sessionKey(r *http.Request, body map[string]json.RawMessage) (string, bool) {
	if id := strings.TrimSpace(r.Header.Get("X-Session-Id")); id != "" {
		return "hdr|" + id, true
	}
	if raw, ok := body["user"]; ok {
		var user string
		if json.Unmarshal(raw, &user) == nil && strings.TrimSpace(user) != "" {
			return "usr|" + user, true
		}
	}
	if raw, ok := body["messages"]; ok {
		var messages []json.RawMessage
		if json.Unmarshal(raw, &messages) == nil {
			for _, m := range messages {
				var head struct {
					Role string `json:"role"`
				}
				if json.Unmarshal(m, &head) == nil && head.Role == "user" {
					return hashedKey("msg|", m)
				}
			}
		}
	}
	// /v1/completions: the prompt is the whole conversation, string or token array.
	if raw, ok := body["prompt"]; ok {
		return hashedKey("msg|", raw)
	}
	return "", false
}

// hashedKey canonicalizes a JSON value (sorted keys, compact) before hashing, so
// two semantically equal payloads map to the same slot.
func hashedKey(prefix string, raw json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(canonical)
	return prefix + hex.EncodeToString(sum[:]), true
}

// slotFileName mixes the session key with the config fingerprint, so a slot is
// only ever restored by the same conversation under the same serving spec.
func slotFileName(sessionKey, fingerprint string) string {
	sum := sha256.Sum256([]byte(sessionKey + "|" + fingerprint))
	return slotPrefix + hex.EncodeToString(sum[:])[:slotHashLen] + slotSuffix
}

// propsFields are the fingerprint components read from the running server.
type propsFields struct {
	NCtx               string
	ModelPath          string
	BuildInfo          string
	ChatTemplateSHA256 string
}

// fingerprintInput is the canonical object hashed into the fingerprint. Field names
// and order are part of the on-disk naming contract: changing either renames every
// slot file, which is harmless because the sweeper reclaims the old ones.
type fingerprintInput struct {
	ImageTag                string `json:"image_tag"`
	ModelRef                string `json:"model_ref"`
	CtxSize                 string `json:"ctx_size"`
	CacheTypeK              string `json:"cache_type_k"`
	CacheTypeV              string `json:"cache_type_v"`
	FlashAttn               string `json:"flash_attn"`
	PropsNCtx               string `json:"props_n_ctx"`
	PropsModelPath          string `json:"props_model_path"`
	PropsBuildInfo          string `json:"props_build_info"`
	PropsChatTemplateSHA256 string `json:"props_chat_template_sha256"`
}

func (b *broker) fingerprint(ctx context.Context) (string, bool) {
	fields, ok := b.props(ctx)
	if !ok {
		return "", false
	}
	encoded, err := json.Marshal(fingerprintInput{
		ImageTag:                b.cfg.imageTag,
		ModelRef:                b.cfg.modelRef,
		CtxSize:                 b.cfg.ctxSize,
		CacheTypeK:              b.cfg.cacheTypeK,
		CacheTypeV:              b.cfg.cacheTypeV,
		FlashAttn:               b.cfg.flashAttn,
		PropsNCtx:               fields.NCtx,
		PropsModelPath:          fields.ModelPath,
		PropsBuildInfo:          fields.BuildInfo,
		PropsChatTemplateSHA256: fields.ChatTemplateSHA256,
	})
	if err != nil {
		log.Printf("warn: fingerprint encode: %v", err)
		return "", false
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), true
}

// propsInput mirrors the /props fields the fingerprint needs. Wire-shaped fields
// (RawMessage) keep a missing key simply absent instead of a zero value that could
// be confused with a real one.
type propsInput struct {
	BuildInfo                 string          `json:"build_info"`
	ModelPath                 string          `json:"model_path"`
	ChatTemplate              json.RawMessage `json:"chat_template"`
	DefaultGenerationSettings struct {
		NCtx json.RawMessage `json:"n_ctx"`
	} `json:"default_generation_settings"`
}

// props returns the cached /props fingerprint inputs, refetching once the TTL has
// lapsed. Failures are never cached: a restarted server must not stay invisible
// for a full TTL.
func (b *broker) props(ctx context.Context) (propsFields, bool) {
	b.propsMu.Lock()
	if b.propsOK && time.Since(b.propsAt) <= b.cfg.propsTTL {
		fields := b.propsCache
		b.propsMu.Unlock()
		return fields, true
	}
	b.propsMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, propsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.upstreamURL+"/props", nil)
	if err != nil {
		return b.propsFailed(err)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return b.propsFailed(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return b.propsFailed(fmt.Errorf("status %d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return b.propsFailed(fmt.Errorf("read: %w", err))
	}
	var in propsInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return b.propsFailed(fmt.Errorf("decode: %w", err))
	}

	fields := propsFields{
		NCtx:      strings.TrimSpace(string(in.DefaultGenerationSettings.NCtx)),
		ModelPath: in.ModelPath,
		BuildInfo: in.BuildInfo,
	}
	if len(in.ChatTemplate) > 0 {
		// chat_template is a JSON string on current builds; anything else is
		// hashed in its wire form so the fingerprint stays well-defined.
		template := []byte(in.ChatTemplate)
		var text string
		if err := json.Unmarshal(in.ChatTemplate, &text); err == nil {
			template = []byte(text)
		}
		sum := sha256.Sum256(template)
		fields.ChatTemplateSHA256 = hex.EncodeToString(sum[:])
	}

	b.propsMu.Lock()
	b.propsAt, b.propsOK, b.propsCache = time.Now(), true, fields
	b.propsMu.Unlock()
	return fields, true
}

// propsFailed records the upstream failure and returns the empty result, so every
// failure branch in props stays a single line. A failed fingerprint means the
// request runs proxied without slots; it is never cached.
func (b *broker) propsFailed(err error) (propsFields, bool) {
	b.metrics.incUpstreamError("props")
	log.Printf("warn: /props: %v", err)
	return propsFields{}, false
}

// slotInfo is the slice of /slots this broker reads. Pointers keep an absent field
// distinguishable from a real zero: a freshly started server's first task really is
// id_task 0, so a missing id_task must not be mistaken for one.
type slotInfo struct {
	ID     *int64 `json:"id"`
	IDTask *int64 `json:"id_task"`
}

// observeSlotTaskLocked polls the live slot list and turns a decreasing id_task
// into a one-shot restore obligation for every slot file on disk. It runs only
// in restore mode auto — the /slots probe is skipped entirely in modes off and
// always, so neither this call nor kv_broker_server_restarts_total moves there.
//
// WHY THE DECREASE IS THE SIGNAL — id_task only grows inside one server process,
// and a restart resets it to a low value. Only a restart loses the in-memory cache
// the slot files exist to replace; on a warm slot the restore is inert and destroys
// the cache the server was about to reuse (measured on the P100), so nothing may
// restore while the counter keeps growing.
//
// Every failure path leaves the gate exactly as it was: the probe is an
// optimization input, never a precondition for answering the request.
func (b *broker) observeSlotTaskLocked(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, slotProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.upstreamURL+"/slots", nil)
	if err != nil {
		log.Printf("warn: /slots probe: %v", err)
		return
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.metrics.incUpstreamError("slots")
		log.Printf("warn: /slots probe: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b.metrics.incUpstreamError("slots")
		log.Printf("warn: /slots probe: status %d", resp.StatusCode)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		b.metrics.incUpstreamError("slots")
		log.Printf("warn: /slots probe: read: %v", err)
		return
	}
	var slots []slotInfo
	if err := json.Unmarshal(raw, &slots); err != nil {
		b.metrics.incUpstreamError("slots")
		log.Printf("warn: /slots probe: decode: %v", err)
		return
	}
	var observed *int64
	for i := range slots {
		if slots[i].ID != nil && *slots[i].ID == 0 {
			observed = slots[i].IDTask
			break
		}
	}
	if observed == nil {
		// A server whose slot list carries no id_task cannot be watched for
		// restarts. Warn (the build may have changed) but do not count it as an
		// upstream error: it would tick on every single request forever.
		log.Printf("warn: /slots probe: no slot 0 with an id_task in %s", short(raw))
		return
	}
	if prev := b.lastSlotTaskID; prev > 0 && *observed < prev {
		b.markRestorePendingLocked()
		b.metrics.incServerRestart()
		log.Printf("info: serving server restarted (slot 0 id_task %d -> %d), %d slot file(s) owe a one-shot restore",
			prev, *observed, len(b.pendingRestore))
	}
	// Never the maximum: the decrease is the signal, so the observed value is kept
	// verbatim even when it looks stale.
	b.lastSlotTaskID = *observed
}

// markRestorePendingLocked schedules one restore for every slot file the
// directory currently holds, so each conversation pays for the restart once
// instead of discovering it on its next request. A directory read failure
// leaves the existing obligations alone rather than silently dropping them.
// Restart detection itself only runs in restore mode auto; the comment block
// above the mode switch in transactional records why the shipped default is
// off.
func (b *broker) markRestorePendingLocked() {
	entries, err := os.ReadDir(b.cfg.slotPath)
	if err != nil {
		log.Printf("warn: restart sweep: read %s: %v", b.cfg.slotPath, err)
		return
	}
	pending := make(map[string]bool)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, slotPrefix) || !strings.HasSuffix(name, slotSuffix) {
			continue
		}
		pending[name] = true
	}
	b.pendingRestore = pending
}

// slotOp performs one slot endpoint call. A failure is a transport error, a
// non-2xx status, or a 2xx body carrying an `error` field — llama.cpp reports a
// failed restore either way.
func (b *broker) slotOp(ctx context.Context, action, filename string) error {
	ctx, cancel := context.WithTimeout(ctx, slotOpTimeout)
	defer cancel()
	status, err := b.slotCall(ctx, b.slotForm, action, filename)
	if err == nil {
		return nil
	}
	if status == http.StatusNotFound && b.slotForm == slotFormQuery {
		// The documented shape is `?action=`; builds that only route
		// `/slots/{id}/{action}` answer 404 instead. Remember what works so all
		// three verbs keep answering.
		log.Printf("info: /slots/0?action=%s answered 404, switching to the path form", action)
		b.slotForm = slotFormPath
		_, err = b.slotCall(ctx, slotFormPath, action, filename)
	}
	if err != nil {
		b.metrics.incUpstreamError("slots")
	}
	return err
}

func (b *broker) slotCall(ctx context.Context, form slotForm, action, filename string) (int, error) {
	payload, err := json.Marshal(map[string]string{"filename": filename})
	if err != nil {
		return 0, err
	}
	target := b.cfg.upstreamURL + "/slots/0?action=" + action
	if form == slotFormPath {
		target = b.cfg.upstreamURL + "/slots/0/" + action
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, short(body))
	}
	var out struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err == nil && len(out.Error) > 0 && string(out.Error) != "null" {
		return resp.StatusCode, fmt.Errorf("error: %s", short(out.Error))
	}
	return resp.StatusCode, nil
}

// short keeps upstream error bodies out of the log at full length.
func short(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// sweepLoop enforces retention periodically; the startup sweep in main covers the
// pod-restart case.
func (b *broker) sweepLoop() {
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	for range ticker.C {
		b.sweep()
	}
}

// sweep applies the retention policy under the transaction mutex, which is what
// keeps a slot whose session is in flight from being reclaimed underneath it.
func (b *broker) sweep() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepLocked()
}

func (b *broker) sweepLocked() {
	entries, err := os.ReadDir(b.cfg.slotPath)
	if err != nil {
		log.Printf("warn: sweep: read %s: %v", b.cfg.slotPath, err)
		return
	}
	type slotFile struct {
		name string
		size int64
		mod  time.Time
	}
	var files []slotFile
	for _, e := range entries {
		name := e.Name()
		// Only slot files are the broker's to reclaim; anything else in the
		// directory belongs to someone else.
		if e.IsDir() || !strings.HasPrefix(name, slotPrefix) || !strings.HasSuffix(name, slotSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			log.Printf("warn: sweep: stat %s: %v", name, err)
			continue
		}
		files = append(files, slotFile{name: name, size: info.Size(), mod: info.ModTime()})
	}

	now := time.Now()
	kept := files[:0]
	var total int64
	for _, f := range files {
		if age := now.Sub(f.mod); age > b.cfg.ttl {
			if b.removeSlot(f.name, fmt.Sprintf("mtime %s older than ttl %s", age.Truncate(time.Second), b.cfg.ttl)) {
				b.metrics.incSweepDeleted("ttl")
				continue
			}
		}
		kept = append(kept, f)
		total += f.size
	}
	if total > b.cfg.capBytes {
		sort.Slice(kept, func(i, j int) bool { return kept[i].mod.Before(kept[j].mod) })
		remaining := len(kept)
		for _, f := range kept {
			if total <= b.cfg.capBytes {
				break
			}
			if b.removeSlot(f.name, fmt.Sprintf("total %d exceeds cap %d", total, b.cfg.capBytes)) {
				b.metrics.incSweepDeleted("cap")
				total -= f.size
				remaining--
			}
		}
		kept = kept[:remaining]
	}
	// The gauges describe what the directory holds now, which is exactly what the
	// scan just established — including after a failed removal.
	b.metrics.setSlotStats(int64(len(kept)), total)
}

// removeSlot reports whether the file is actually gone, so a failed removal is not
// counted as a deletion or subtracted from the size gauges.
func (b *broker) removeSlot(name, reason string) bool {
	if err := os.Remove(filepath.Join(b.cfg.slotPath, name)); err != nil {
		log.Printf("warn: sweep: remove %s: %v", name, err)
		return false
	}
	log.Printf("sweep: deleted %s (%s)", name, reason)
	return true
}

// forwardRequestHeaders copies the end-to-end request headers. The body is
// re-encoded, so Content-Length and the transfer framing stay with the broker.
func forwardRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] || http.CanonicalHeaderKey(k) == "Content-Length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyResponseHeaders copies end-to-end response headers; the upstream body is
// streamed verbatim, so its Content-Length stays meaningful.
func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// flushingWriter pushes every chunk to the client immediately: an SSE stream must
// not wait for the copy buffer to fill.
type flushingWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f *flushingWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		_ = f.rc.Flush()
	}
	return n, err
}
