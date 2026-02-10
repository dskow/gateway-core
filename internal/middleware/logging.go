// Package middleware provides common HTTP middleware for the API gateway
// including structured logging, CORS, and panic recovery.
package middleware

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LogLevelNone is a sentinel value indicating no log entry should be emitted.
// It is higher than any slog.Level so logger.Enabled() will always return false.
const LogLevelNone slog.Level = slog.LevelError + 100

// ParseLogLevel converts a route log_level string to a slog.Level.
// Returns slog.LevelInfo for empty string (default).
func ParseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "none":
		return LogLevelNone
	default:
		return slog.LevelInfo
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.statusCode = code
	sr.ResponseWriter.WriteHeader(code)
}

// LoggingConfig holds the runtime options for the Logging middleware.
type LoggingConfig struct {
	BodyLogging     bool
	MaxBodyLogBytes int
}

// Logging returns middleware that logs each request as structured JSON
// including method, path, status code, latency, and client IP.
// routeLogLevel maps a request path to its configured log level; pass nil
// for the default (Info for all requests). bodyConfig enables opt-in body
// logging when non-nil.
func Logging(logger *slog.Logger, routeLogLevel func(string) slog.Level, bodyConfig *LoggingConfig) func(http.Handler) http.Handler {
	if routeLogLevel == nil {
		routeLogLevel = func(string) slog.Level { return slog.LevelInfo }
	}

	logBody := bodyConfig != nil && bodyConfig.BodyLogging
	maxBody := 4096
	if bodyConfig != nil && bodyConfig.MaxBodyLogBytes > 0 {
		maxBody = bodyConfig.MaxBodyLogBytes
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			level := routeLogLevel(r.URL.Path)

			// Skip logging entirely for "none" routes.
			if level == LogLevelNone {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()

			var reqBody string
			if logBody && shouldLogBody(r.Header.Get("Content-Type")) && r.Body != nil {
				reqBody = captureRequestBody(r, maxBody)
			}

			var recorder *statusRecorder
			var respCapture *bodyCapture

			if logBody && shouldLogBody("") { // we don't know response content-type yet
				respCapture = bodyCapturePool.Get().(*bodyCapture)
				respCapture.Reset()
				respCapture.maxBytes = maxBody
				recorder = &statusRecorder{ResponseWriter: &bodyRecorder{ResponseWriter: w, capture: respCapture}, statusCode: http.StatusOK}
			} else {
				recorder = &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
			}

			next.ServeHTTP(recorder, r)

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.statusCode,
				"latency_ms", time.Since(start).Milliseconds(),
				"client_ip", r.RemoteAddr,
				"request_id", GetRequestID(r.Context()),
			}

			if reqBody != "" {
				attrs = append(attrs, "request_body", reqBody)
			}
			if respCapture != nil && shouldLogBody(respCapture.contentType) {
				body := respCapture.String()
				if body != "" {
					attrs = append(attrs, "response_body", redactSensitive(body))
				}
			}

			logger.Log(r.Context(), level, "request", attrs...)

			// Return body capture buffer to pool after logging.
			if respCapture != nil {
				bodyCapturePool.Put(respCapture)
			}
		})
	}
}

// shouldLogBody returns true if the content type is text-based.
func shouldLogBody(contentType string) bool {
	if contentType == "" {
		return true // will be filtered later when we know the content type
	}
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "json") ||
		strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "xml") ||
		strings.Contains(ct, "form-urlencoded")
}

// captureRequestBody reads and replaces r.Body, returning up to maxBytes
// of the body as a string.
func captureRequestBody(r *http.Request, maxBytes int) string {
	var buf bytes.Buffer
	tee := io.TeeReader(r.Body, &buf)
	limited := io.LimitReader(tee, int64(maxBytes)+1)
	captured, _ := io.ReadAll(limited)
	// Reconstruct body for downstream handlers.
	r.Body = io.NopCloser(io.MultiReader(&buf, r.Body))

	s := string(captured)
	if len(captured) > maxBytes {
		s = s[:maxBytes] + "...[truncated]"
	}
	return redactSensitive(s)
}

// sensitiveFieldRe matches JSON key-value pairs for common sensitive fields.
// Compiled once at package init — single-pass replacement avoids the O(n·k²)
// cost of the previous approach that re-lowered the entire string per field.
var sensitiveFieldRe = regexp.MustCompile(
	`(?i)"(?:password|secret|token|key|authorization)"\s*:\s*"[^"]*"`,
)

// redactSensitive replaces common sensitive field values in log output.
// Uses a compiled regex for single-pass replacement instead of iterating
// per-field with repeated ToLower calls.
func redactSensitive(s string) string {
	return sensitiveFieldRe.ReplaceAllStringFunc(s, func(match string) string {
		// Find the last `:"` pattern to locate the value portion.
		colonQuote := strings.LastIndex(match, `"`)
		// Walk backwards to find the opening quote of the value.
		inner := match[:colonQuote]
		valueOpen := strings.LastIndex(inner, `"`)
		if valueOpen == -1 {
			return match
		}
		return match[:valueOpen+1] + "***" + `"`
	})
}

// bodyCapturePool reuses bodyCapture structs to reduce GC pressure in the
// logging hot path. Each request with body logging enabled gets/puts one.
var bodyCapturePool = sync.Pool{
	New: func() interface{} { return &bodyCapture{} },
}

// bodyCapture collects response body bytes up to a limit.
type bodyCapture struct {
	buf         bytes.Buffer
	maxBytes    int
	contentType string
}

// Reset clears the bodyCapture for reuse via the pool.
func (bc *bodyCapture) Reset() {
	bc.buf.Reset()
	bc.maxBytes = 0
	bc.contentType = ""
}

func (bc *bodyCapture) Write(p []byte) {
	remaining := bc.maxBytes - bc.buf.Len()
	if remaining <= 0 {
		return
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	bc.buf.Write(p)
}

func (bc *bodyCapture) String() string {
	return bc.buf.String()
}

// bodyRecorder wraps ResponseWriter to capture response body bytes.
type bodyRecorder struct {
	http.ResponseWriter
	capture       *bodyCapture
	headerWritten bool
}

func (br *bodyRecorder) WriteHeader(code int) {
	if !br.headerWritten {
		br.headerWritten = true
		br.capture.contentType = br.ResponseWriter.Header().Get("Content-Type")
	}
	br.ResponseWriter.WriteHeader(code)
}

func (br *bodyRecorder) Write(b []byte) (int, error) {
	if !br.headerWritten {
		br.headerWritten = true
		br.capture.contentType = br.ResponseWriter.Header().Get("Content-Type")
	}
	br.capture.Write(b)
	return br.ResponseWriter.Write(b)
}
