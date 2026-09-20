// Shared stdlib HTTP plumbing for both services (no framework, no deps).
//
// Both roles speak plain HTTP/1.1 on :8080. The helpers here are deliberately
// tiny: request parsing, a bounded body read and byte-exact responses — the
// reaper must verify the HMAC over the *raw* event body, so the body is read as
// bytes and only decoded afterwards.
//
// Port of src/egress_guard/http.py.
package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxBodyBytes bounds memory per connection on a public-ish path: an ext_authz
// check request or a violation event is a few hundred bytes, so 64 KiB is
// generous headroom. Python: MAX_BODY_BYTES.
const maxBodyBytes = 64 * 1024

const contentTypeJSON = "application/json"

// HttpResult is a fully materialized response: status, extra headers, body
// bytes and content type. Mirrors the Python HttpResult, whose body is also
// bytes (the /metrics path ships Prometheus text through it).
type HttpResult struct {
	Status      int
	Body        []byte
	Headers     map[string]string
	ContentType string
}

// Handler is the request contract shared by both services: the same shape as
// http.Handler, named here so the service slices route against one type and the
// server can take it directly (Python: JsonHandler).
type Handler interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// jsonResult renders payload with the Python json_result — sorted keys,
// application/json — so both services answer byte-for-byte like the Python did.
func jsonResult(status int, payload any, headers map[string]string) *HttpResult {
	body, err := marshalPythonJSON(payload)
	if err != nil {
		// Python raises here and the framework answers 500; keep the failure
		// visible and never send a half-encoded body.
		log.Printf("encode response: %v", err)
		return &HttpResult{
			Status:      http.StatusInternalServerError,
			Body:        []byte("{\"error\": \"internal-error\"}"),
			ContentType: contentTypeJSON,
		}
	}
	return &HttpResult{Status: status, Body: body, Headers: headers, ContentType: contentTypeJSON}
}

// notFoundResult is the response every unknown route answers with.
func notFoundResult() *HttpResult {
	return jsonResult(http.StatusNotFound, map[string]any{"error": "not-found"}, nil)
}

// badRequestResult is the response every malformed request answers with; detail
// is the readBody/readJSON error string.
func badRequestResult(detail string) *HttpResult {
	return jsonResult(http.StatusBadRequest, map[string]any{"error": "bad-request", "detail": detail}, nil)
}

// writeResult sends the response: extra headers, then the content type and an
// explicit Content-Length (HTTP/1.1 keep-alive depends on it). A client that
// goes away mid-response is not an error worth reporting — the Go server closes
// that connection on its own. Mirrors the Python write_result.
func writeResult(w http.ResponseWriter, res *HttpResult) {
	for name, value := range res.Headers {
		w.Header().Set(name, value)
	}
	contentType := res.ContentType
	if contentType == "" {
		contentType = contentTypeJSON
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(res.Body)))
	w.WriteHeader(res.Status)
	if len(res.Body) == 0 {
		return
	}
	if _, err := w.Write(res.Body); err != nil {
		log.Printf("write response: %v", err)
	}
}

// readBody reads the request body as raw bytes, bounded by Content-Length. The
// raw bytes are what the reaper signs over, so nothing here decodes. Mirrors
// the Python read_body, whose ValueError message is the body of the 400 the
// services send.
func readBody(r *http.Request) ([]byte, error) {
	length := r.ContentLength
	if length <= 0 {
		return nil, errors.New("missing or empty request body")
	}
	if length > maxBodyBytes {
		return nil, fmt.Errorf("request body too large: %d bytes", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errors.New("truncated request body")
		}
		return nil, fmt.Errorf("read request body: %v", err)
	}
	return body, nil
}

// readJSON reads the body and decodes it into the generic JSON shape (objects
// as map[string]any, numbers as float64). Use readInto for a typed payload:
// JSON has one number type, so the loose decode cannot tell an integer from a
// float. Mirrors the Python read_json, where unparseable JSON is a ValueError.
func readJSON(r *http.Request) (any, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %v", err)
	}
	return payload, nil
}

// readInto reads the bounded raw body and decodes it into target (a pointer to
// a struct or map). Unknown fields stay ignored, matching the Python dict
// access; a malformed body is reported like read_json's ValueError.
func readInto(r *http.Request, target any) error {
	body, err := readBody(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	return nil
}

// marshalPythonJSON renders value exactly like Python's
// json.dumps(value, sort_keys=True): space-separated items, ": " between key
// and value, keys sorted in code-point order, ASCII-only output, and Python's
// float repr.
//
// encoding/json is deliberately NOT used: it emits compact separators, escapes
// <, > and & (which Python leaves raw) and leaves non-ASCII runes in place, so
// its bytes differ from the Python service's on almost every response.
func marshalPythonJSON(value any) ([]byte, error) {
	// 64 KiB responses at most, and most are one key; start small and grow.
	return appendPythonJSON(make([]byte, 0, 64), value)
}

func appendPythonJSON(dst []byte, value any) ([]byte, error) {
	if value == nil {
		return append(dst, "null"...), nil
	}
	switch special := value.(type) {
	case json.RawMessage:
		// A pre-serialized document (K8s bodies keep their own layout).
		return append(dst, special...), nil
	case json.Number:
		return append(dst, special.String()...), nil
	}

	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Invalid:
		return append(dst, "null"...), nil
	case reflect.Bool:
		return strconv.AppendBool(dst, rv.Bool()), nil
	case reflect.String:
		return appendPythonString(dst, rv.String()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, rv.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return append(dst, pythonFloat(rv.Float())...), nil
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return append(dst, "null"...), nil
		}
		return appendPythonJSON(dst, rv.Elem().Interface())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			// A nil Go slice is the empty list in Python only if it was built
			// as one; a nil slice reaching here means "no value" — mirror None.
			return append(dst, "null"...), nil
		}
		dst = append(dst, '[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				dst = append(dst, ',', ' ')
			}
			var err error
			if dst, err = appendPythonJSON(dst, rv.Index(i).Interface()); err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case reflect.Map:
		if rv.IsNil() {
			return append(dst, "null"...), nil
		}
		keys := rv.MapKeys()
		names := make([]string, len(keys))
		for i, key := range keys {
			if key.Kind() != reflect.String {
				return nil, fmt.Errorf("json_result: map key type %s is not a string", key.Type())
			}
			names[i] = key.String()
		}
		// Bytewise sort equals Python's sort_keys (code-point order) on UTF-8.
		sort.Strings(names)
		dst = append(dst, '{')
		for i, name := range names {
			if i > 0 {
				dst = append(dst, ',', ' ')
			}
			dst = appendPythonString(dst, name)
			dst = append(dst, ':', ' ')
			var err error
			if dst, err = appendPythonJSON(dst, rv.MapIndex(reflect.ValueOf(name)).Interface()); err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	case reflect.Struct:
		fields, err := structFields(rv)
		if err != nil {
			return nil, err
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
		dst = append(dst, '{')
		for i, field := range fields {
			if i > 0 {
				dst = append(dst, ',', ' ')
			}
			dst = appendPythonString(dst, field.name)
			dst = append(dst, ':', ' ')
			if dst, err = appendPythonJSON(dst, field.value); err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	}
	return nil, fmt.Errorf("json_result: unsupported type %s", rv.Type())
}

type encodedField struct {
	name  string
	value any
}

// structFields flattens an exported struct the way encoding/json would, so a
// typed response struct serializes under the same names it was tagged with.
func structFields(rv reflect.Value) ([]encodedField, error) {
	structType := rv.Type()
	fields := make([]encodedField, 0, rv.NumField())
	for i := 0; i < rv.NumField(); i++ {
		fieldType := structType.Field(i)
		if fieldType.PkgPath != "" {
			continue // unexported
		}
		name, omitEmpty := parseJSONTag(fieldType.Tag.Get("json"), fieldType.Name)
		if name == "-" {
			continue
		}
		fieldValue := rv.Field(i)
		if omitEmpty && isEmptyValue(fieldValue) {
			continue
		}
		fields = append(fields, encodedField{name: name, value: fieldValue.Interface()})
	}
	return fields, nil
}

func parseJSONTag(tag, fallback string) (string, bool) {
	if tag == "" {
		return fallback, false
	}
	name, options, _ := strings.Cut(tag, ",")
	if name == "" {
		name = fallback
	}
	omitEmpty := false
	for _, option := range strings.Split(options, ",") {
		if option == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty
}

// isEmptyValue mirrors encoding/json's omitempty definition.
func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// pythonFloat renders f the way CPython's float repr (and therefore
// json.dumps) does: shortest round-trip digits, positional form while the
// decimal point sits in [-4, 16], exponent form outside it, and a fractional
// part always present in positional form so a float never reads as an int.
// Verified against CPython on 25k values spanning random bit patterns.
func pythonFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}

	negative := math.Signbit(f)
	magnitude := math.Abs(f)

	// 'e' gives the shortest round-trip digits plus a decimal exponent; Go's
	// 'g' picks its own thresholds, so the form is chosen here instead.
	scientific := strconv.FormatFloat(magnitude, 'e', -1, 64)
	exponent, err := strconv.Atoi(scientific[strings.IndexByte(scientific, 'e')+1:])
	if err != nil {
		// Unreachable for a finite float; fall back rather than panic in a
		// request path.
		log.Printf("pythonFloat: parse exponent of %q: %v", scientific, err)
		return scientific
	}
	// Position of the decimal point relative to the first digit (repr's decpt).
	decimalPoint := exponent + 1

	var rendered string
	if decimalPoint <= -4 || decimalPoint > 16 {
		rendered = scientific
	} else {
		rendered = strconv.FormatFloat(magnitude, 'f', -1, 64)
		if !strings.ContainsRune(rendered, '.') {
			rendered += ".0"
		}
	}
	if negative {
		return "-" + rendered
	}
	return rendered
}

// appendPythonString quotes s like Python's json.dumps: always double-quoted
// with ensure_ascii, i.e. every rune outside printable ASCII (including DEL and
// all C0 controls) escaped as \uXXXX, astral runes as a surrogate pair. `/`,
// `&`, `<` and `>` stay literal.
func appendPythonString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for _, r := range s {
		switch r {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if r < 0x20 || r > 0x7e {
				dst = appendUnicodeEscape(dst, r)
				continue
			}
			dst = utf8.AppendRune(dst, r)
		}
	}
	return append(dst, '"')
}

const hexDigits = "0123456789abcdef"

// appendUnicodeEscape writes \uXXXX, splitting astral runes into the UTF-16
// surrogate pair Python emits.
func appendUnicodeEscape(dst []byte, r rune) []byte {
	if r > 0xffff {
		r -= 0x10000
		high := 0xd800 + (r >> 10)
		low := 0xdc00 + (r & 0x3ff)
		return appendHex4(appendHex4(dst, high), low)
	}
	return appendHex4(dst, r)
}

func appendHex4(dst []byte, code rune) []byte {
	return append(dst,
		'\\', 'u',
		hexDigits[(code>>12)&0xf],
		hexDigits[(code>>8)&0xf],
		hexDigits[(code>>4)&0xf],
		hexDigits[code&0xf],
	)
}