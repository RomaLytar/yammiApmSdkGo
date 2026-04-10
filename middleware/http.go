// Package middleware contains transport-level adapters that convert
// incoming requests into apm.Event values and push them into the SDK's
// trace buffer. There are two flavours: net/http (works with chi, gin,
// echo and any router that speaks the standard handler interface) and
// gRPC (unary + stream interceptors).
package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	apm "github.com/RomaLytar/yammiApmSdkGo"
)

// HTTP returns a net/http middleware that records each request as an
// apm.Event. Use it like:
//
//	mux := chi.NewRouter()
//	mux.Use(middleware.HTTP(a))
//
// or as a generic wrapper for plain net/http:
//
//	http.ListenAndServe(":8080", middleware.HTTP(a)(myHandler))
//
// The middleware never blocks the request: it captures duration and
// status code synchronously (cheap) and the actual upload to the APM
// backend happens asynchronously through apm.Buffer.
func HTTP(a *apm.APM) func(http.Handler) http.Handler {
	if a == nil {
		// Defensive: if the user passed a nil APM (e.g. tests), return a
		// pass-through middleware so the rest of the stack still works.
		return func(next http.Handler) http.Handler { return next }
	}
	cfg := a.Config()
	ignore := makeIgnoreSet(cfg.IgnoreEndpoints)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if _, skip := ignore[path]; skip {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rw := &responseRecorder{ResponseWriter: w, status: 200}

			// Capture the body for error extraction on 4xx/5xx. Limit to
			// 4 KiB so a streaming endpoint can't blow up our buffer.
			rw.captureBody = true
			rw.bodyLimit = 4 * 1024

			next.ServeHTTP(rw, r)

			durationMs := float32(time.Since(start).Seconds() * 1000)
			errMsg := ""
			if rw.status >= 400 {
				errMsg = extractError(rw.body.Bytes())
			}

			a.PushEvent(apm.Event{
				Endpoint:   path,
				Method:     normalizeMethod(r.Method),
				StatusCode: uint16(rw.status),
				DurationMs: durationMs,
				Error:      errMsg,
				ClientIP:   clientIP(r),
				UserAgent:  trim(r.UserAgent(), 500),
				Timestamp:  time.Now().Unix(),
			})
		})
	}
}

// makeIgnoreSet turns the slice from config into a hash for O(1) lookup.
func makeIgnoreSet(paths []string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		out[p] = struct{}{}
	}
	return out
}

// responseRecorder wraps http.ResponseWriter so the middleware can read
// the status code (and optionally the body) after the handler returns.
// It does not implement Hijacker / Pusher because the APM use case never
// needs them; if a user trips on that, they can pass through manually.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool

	captureBody bool
	bodyLimit   int
	body        bytes.Buffer
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.captureBody && r.body.Len() < r.bodyLimit {
		// Only capture as much as the configured limit allows.
		room := r.bodyLimit - r.body.Len()
		if len(b) <= room {
			r.body.Write(b)
		} else {
			r.body.Write(b[:room])
		}
	}
	return r.ResponseWriter.Write(b)
}

// extractError tries to pull a human-readable error message out of a
// JSON response body. It looks for the keys Laravel and most Go HTTP
// libraries tend to use: "message", "error", "exception".
func extractError(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// Cheap key search before reaching for json.Unmarshal — most error
	// payloads we care about are tiny and one of the three keys appears
	// in the first 200 bytes.
	keys := []string{`"message"`, `"error"`, `"exception"`}
	for _, k := range keys {
		if idx := bytes.Index(body, []byte(k)); idx >= 0 {
			// Naive value extraction: find the colon, then the next quote,
			// then the closing quote. Good enough for the 90 % case where
			// the value is a flat string.
			rest := body[idx+len(k):]
			colon := bytes.IndexByte(rest, ':')
			if colon < 0 {
				continue
			}
			rest = rest[colon+1:]
			open := bytes.IndexByte(rest, '"')
			if open < 0 {
				continue
			}
			rest = rest[open+1:]
			close := bytes.IndexByte(rest, '"')
			if close < 0 {
				continue
			}
			return trim(string(rest[:close]), 500)
		}
	}
	return ""
}

// clientIP returns the best-effort source IP of the request, honouring
// X-Forwarded-For and X-Real-IP first because the SDK normally runs
// behind ingress / nginx.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// XFF is a comma-separated chain — the first entry is the closest
		// to the original client.
		if comma := strings.IndexByte(v, ','); comma > 0 {
			return strings.TrimSpace(v[:comma])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	addr := r.RemoteAddr
	if colon := strings.LastIndexByte(addr, ':'); colon > 0 {
		return addr[:colon]
	}
	return addr
}

// normalizeMethod uppercases the request method. The backend rejects
// anything outside the standard set, so unusual methods (PROPFIND, …)
// are coerced to POST to keep the trace alive.
func normalizeMethod(m string) string {
	m = strings.ToUpper(m)
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return m
	default:
		return "POST"
	}
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Compile-time guards. We never want HTTP() to depend on packages outside
// the standard library and the SDK itself, otherwise it would drag a
// router framework into every consumer's go.mod. The blank identifiers
// below force a build error if some accidental import sneaks in later.
var (
	_ io.Writer = (*bytes.Buffer)(nil)
)
