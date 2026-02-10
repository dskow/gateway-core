// Package apierror provides a centralized error response format for the API
// gateway. All gateway components use WriteJSON to produce consistent,
// machine-readable error responses with stable error codes.
package apierror

import (
	"encoding/json"
	"net/http"
)

// ErrorCode is a machine-readable error classification string.
type ErrorCode string

// Gateway error codes. These form a public API contract — clients can program
// against these stable codes. Do not rename or remove existing codes.
const (
	RouteNotFound         ErrorCode = "GATEWAY_ROUTE_NOT_FOUND"
	MethodNotAllowed      ErrorCode = "GATEWAY_METHOD_NOT_ALLOWED"
	UpstreamUnavailable   ErrorCode = "GATEWAY_UPSTREAM_UNAVAILABLE"
	CircuitOpen           ErrorCode = "GATEWAY_CIRCUIT_OPEN"
	RequestCancelled      ErrorCode = "GATEWAY_REQUEST_CANCELLED"
	AuthMissingToken      ErrorCode = "GATEWAY_AUTH_MISSING_TOKEN"
	AuthInvalidToken      ErrorCode = "GATEWAY_AUTH_INVALID_TOKEN"
	AuthInsufficientScope ErrorCode = "GATEWAY_AUTH_INSUFFICIENT_SCOPE"
	RateLimitExceeded     ErrorCode = "GATEWAY_RATE_LIMIT_EXCEEDED"
	InternalError         ErrorCode = "GATEWAY_INTERNAL_ERROR"
	BodyTooLarge          ErrorCode = "GATEWAY_BODY_TOO_LARGE"
	DeadlineExceeded      ErrorCode = "GATEWAY_DEADLINE_EXCEEDED"
)

// ErrorResponse is the standardized gateway error body.
type ErrorResponse struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// Pre-serialized JSON bodies for the most common error responses.
// Avoids json.Encoder allocation on every error in the hot path.
// These do NOT include request_id since it varies per request.
var (
	preRouteNotFound       = mustMarshal(http.StatusNotFound, RouteNotFound, "no matching route")
	preUpstreamUnavailable = mustMarshal(http.StatusBadGateway, UpstreamUnavailable, "upstream service unavailable")
	preCircuitOpen         = mustMarshal(http.StatusServiceUnavailable, CircuitOpen, "circuit breaker open")
	preRequestCancelled    = mustMarshal(http.StatusGatewayTimeout, RequestCancelled, "request cancelled")
	preAuthMissingToken    = mustMarshal(http.StatusUnauthorized, AuthMissingToken, "missing or malformed Authorization header")
	preRateLimitExceeded   = mustMarshal(http.StatusTooManyRequests, RateLimitExceeded, "rate limit exceeded, retry later")
)

func mustMarshal(status int, code ErrorCode, message string) []byte {
	b, _ := json.Marshal(ErrorResponse{
		Error:     http.StatusText(status),
		ErrorCode: string(code),
		Message:   message,
	})
	return append(b, '\n')
}

// WriteJSON writes a structured JSON error response. For common error
// code+message combinations, pre-serialized bodies are used (no allocation).
// When request_id is available (from X-Request-ID header), it is included in
// the response. The request parameter may be nil for contexts where the
// request is not available.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, code ErrorCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// Fast path: use pre-serialized body for common errors when there is
	// no request ID to include (avoids allocation).
	requestID := ""
	if r != nil {
		requestID = r.Header.Get("X-Request-ID")
	}

	if requestID == "" {
		if body := preSerialized(status, code, message); body != nil {
			w.Write(body) //nolint:errcheck
			return
		}
	}

	json.NewEncoder(w).Encode(ErrorResponse{
		Error:     http.StatusText(status),
		ErrorCode: string(code),
		Message:   message,
		RequestID: requestID,
	})
}

// preSerialized returns a pre-built response body for common error
// combinations, or nil if no match.
func preSerialized(status int, code ErrorCode, message string) []byte {
	switch {
	case code == RouteNotFound && status == http.StatusNotFound && message == "no matching route":
		return preRouteNotFound
	case code == UpstreamUnavailable && status == http.StatusBadGateway && message == "upstream service unavailable":
		return preUpstreamUnavailable
	case code == CircuitOpen && status == http.StatusServiceUnavailable && message == "circuit breaker open":
		return preCircuitOpen
	case code == RequestCancelled && status == http.StatusGatewayTimeout && message == "request cancelled":
		return preRequestCancelled
	case code == AuthMissingToken && status == http.StatusUnauthorized && message == "missing or malformed Authorization header":
		return preAuthMissingToken
	case code == RateLimitExceeded && status == http.StatusTooManyRequests && message == "rate limit exceeded, retry later":
		return preRateLimitExceeded
	}
	return nil
}
