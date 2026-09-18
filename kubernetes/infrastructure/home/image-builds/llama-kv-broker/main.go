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
	sweepEvery   = 24 * time.Hour

	slotPrefix  = "slot-"
	slotSuffix  = ".bin"
	slotHashLen = 20

	actionRestore = "restore"
	actionSave    = "save"
	actionErase   = "erase"
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

	propsTTL time.Duration
	ttl      time.Duration
	capBytes int64
}

func (c config) String() string {
	return fmt.Sprintf(
		"upstream=%s slot_path=%s listen=%s fingerprint=[image=%q model=%q ctx=%q cache_k=%q cache_v=%q flash_attn=%q] props_ttl=%s slot_ttl=%s cap_bytes=%d",
		c.upstreamURL, c.slotPath, c.listenAddr, c.imageTag, c.modelRef, c.ctxSize,
		c.cacheTypeK, c.cacheTypeV, c.flashAttn, c.propsTTL, c.ttl, c.capBytes)
}

func loadConfig() config {
	return config{
		upstreamURL: strings.TrimRight(envStr("UPSTREAM_URL", ""), "/"),
		slotPath:    envStr("SLOT_PATH", defaultSlotPath),
		listenAddr:  envStr("LISTEN_ADDR", defaultListenAddr),
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
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("warn: proxy %s %s: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusBadGateway)
	}
	return &broker{
		cfg:    cfg,
		client: &http.Client{Transport: transport},
		proxy:  proxy,
	}, nil
}

func (b *broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
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
	slotAbs := filepath.Join(b.cfg.slotPath, slot)
	if _, err := os.Stat(slotAbs); err == nil {
		if err := b.slotOp(opCtx, actionRestore, slot); err != nil {
			log.Printf("warn: restore %s failed (%v), erasing the slot and serving cold", slot, err)
			if e := b.slotOp(opCtx, actionErase, slot); e != nil {
				log.Printf("warn: erase %s failed: %v", slot, e)
			}
			if e := os.Remove(slotAbs); e != nil && !os.IsNotExist(e) {
				log.Printf("warn: remove %s failed: %v", slot, e)
			}
		} else {
			log.Printf("restore %s", slot)
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
		return
	}
	if copyErr != nil {
		log.Printf("warn: client went away before EOF, not saving %s: %v", slot, copyErr)
		return
	}
	if err := b.slotOp(opCtx, actionSave, slot); err != nil {
		log.Printf("warn: save %s failed: %v", slot, err)
		return
	}
	log.Printf("save %s", slot)
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
		return propsFields{}, false
	}
	resp, err := b.client.Do(req)
	if err != nil {
		log.Printf("warn: /props: %v", err)
		return propsFields{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("warn: /props: status %d", resp.StatusCode)
		return propsFields{}, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		log.Printf("warn: /props: read: %v", err)
		return propsFields{}, false
	}
	var in propsInput
	if err := json.Unmarshal(raw, &in); err != nil {
		log.Printf("warn: /props: decode: %v", err)
		return propsFields{}, false
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
	var total int64
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
	for _, f := range files {
		if age := now.Sub(f.mod); age > b.cfg.ttl {
			b.removeSlot(f.name, fmt.Sprintf("mtime %s older than ttl %s", age.Truncate(time.Second), b.cfg.ttl))
			continue
		}
		kept = append(kept, f)
		total += f.size
	}
	if total <= b.cfg.capBytes {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].mod.Before(kept[j].mod) })
	for _, f := range kept {
		if total <= b.cfg.capBytes {
			break
		}
		b.removeSlot(f.name, fmt.Sprintf("total %d exceeds cap %d", total, b.cfg.capBytes))
		total -= f.size
	}
}

func (b *broker) removeSlot(name, reason string) {
	if err := os.Remove(filepath.Join(b.cfg.slotPath, name)); err != nil {
		log.Printf("warn: sweep: remove %s: %v", name, err)
		return
	}
	log.Printf("sweep: deleted %s (%s)", name, reason)
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
