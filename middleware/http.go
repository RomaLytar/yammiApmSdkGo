// Package middleware contains transport-level adapters that convert
// incoming requests into apm.Event values and push them into the SDK's
// trace buffer. There are two flavours: net/http (works with chi, gin,
// echo and any router that speaks the standard handler interface) and
// gRPC (unary + stream interceptors).
package middleware

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"

	apm "github.com/RomaLytar/yammiApmSdkGo"
)

// HTTP returns a net/http middleware that records each request as an
// apm.Event and pushes it into the SDK's non-blocking buffer.
//
// Hot-path goals:
//
//  1. O(1) per request, no per-request goroutine, no syscalls beyond
//     what net/http already does.
//  2. responseRecorder is pooled via sync.Pool so per-request cost is
//     a single atomic pointer swap, not a malloc. The 2 KiB error
//     buffer inside each pooled recorder is reused across requests.
//  3. Body capture is automatic for all 4xx and 5xx responses (so the
//     dashboard always has a "why did this fail" snippet), but the
//     buffer is only WRITTEN to on those status codes — 2xx hot path
//     does nothing beyond the status-code check. 4xx bodies usually
//     carry validation details worth showing, 5xx carry the stack
//     trace snippet; both are capped at 2 KiB.
//
// Opt-out: set CaptureErrorBody=false or APM_CAPTURE_ERROR_BODY=false
// if you don't want any body capture at all (saves the 2 KiB buffer
// allocation on first use of a pool slot).
func HTTP(a *apm.APM) func(http.Handler) http.Handler {
	if a == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	cfg := a.Config()
	ignore := makeIgnoreSet(cfg.IgnoreEndpoints)
	captureErrors := cfg.CaptureErrorBody

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if _, skip := ignore[path]; skip {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rw := recorderPool.Get().(*responseRecorder)
			rw.reset(w, captureErrors)

			next.ServeHTTP(rw, r)

			durationMs := float32(time.Since(start).Seconds() * 1000)
			status := rw.status
			errMsg := ""
			// Any 4xx or 5xx gets the captured body parsed for a
			// readable error message. 2xx/3xx never reach this branch,
			// so the successful path pays no extra cost.
			if status >= 400 && rw.captured != nil && rw.captured.Len() > 0 {
				errMsg = extractError(rw.captured.Bytes())
				if errMsg == "" {
					// No recognised JSON key — use the raw first-2-KiB
					// body as-is so the dashboard at least shows
					// something instead of an empty error field.
					errMsg = trim(string(rw.captured.Bytes()), 500)
				}
			}

			a.PushEvent(apm.Event{
				Endpoint:   path,
				Method:     normalizeMethod(r.Method),
				StatusCode: uint16(status),
				DurationMs: durationMs,
				Error:      errMsg,
				ClientIP:   clientIP(r),
				UserAgent:  trim(r.UserAgent(), 500),
				Timestamp:  time.Now().Unix(),
			})

			rw.release()
			recorderPool.Put(rw)
		})
	}
}

// recorderPool reuses responseRecorder structs so the middleware does
// not allocate one per request. Under the 1000-user k6 load the hot
// path allocates nothing on 2xx responses beyond what net/http already
// does for its internal state.
var recorderPool = sync.Pool{
	New: func() any {
		return &responseRecorder{status: http.StatusOK}
	},
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
// the status code (and optionally the body for 5xx) after the handler
// returns. Instances are recycled through recorderPool.
//
// Body capture is opt-in via Reset(captureErrors=true). Even then, the
// capture buffer is allocated lazily on the first write, so 2xx
// responses pay nothing.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool

	captureErrors bool
	captured      *bytes.Buffer
}

func (r *responseRecorder) reset(w http.ResponseWriter, captureErrors bool) {
	r.ResponseWriter = w
	r.status = http.StatusOK
	r.wroteHeader = false
	r.captureErrors = captureErrors
	if r.captured != nil {
		r.captured.Reset()
	}
}

func (r *responseRecorder) release() {
	r.ResponseWriter = nil
	if r.captured != nil && r.captured.Cap() > 8192 {
		// Don't keep oversized buffers in the pool — they'd amplify
		// memory use if a single handler once wrote a huge error body.
		r.captured = nil
	}
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
	// Lazy capture: only when the host opted in AND the response is
	// 4xx/5xx. The 2xx hot path checks one integer and falls through
	// with zero work — no allocation, no branch mispredictions beyond
	// the comparison itself.
	if r.captureErrors && r.status >= 400 {
		if r.captured == nil {
			r.captured = bytes.NewBuffer(make([]byte, 0, 2048))
		}
		if r.captured.Len() < 2048 {
			room := 2048 - r.captured.Len()
			if len(b) <= room {
				r.captured.Write(b)
			} else {
				r.captured.Write(b[:room])
			}
		}
	}
	return r.ResponseWriter.Write(b)
}

// Flush makes responseRecorder satisfy http.Flusher so streaming
// handlers (SSE, chunked) keep working.
func (r *responseRecorder) Flush() {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// extractError tries to pull a human-readable error message out of a
// JSON response body. It looks for the keys Laravel and most Go HTTP
// libraries tend to use: "message", "error", "exception".
func extractError(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	keys := []string{`"message"`, `"error"`, `"exception"`}
	for _, k := range keys {
		if idx := bytes.Index(body, []byte(k)); idx >= 0 {
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
			closeIdx := bytes.IndexByte(rest, '"')
			if closeIdx < 0 {
				continue
			}
			return trim(string(rest[:closeIdx]), 500)
		}
	}
	return ""
}

// clientIP returns the best-effort source IP of the request, honouring
// X-Forwarded-For and X-Real-IP first because the SDK normally runs
// behind ingress / nginx.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
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
