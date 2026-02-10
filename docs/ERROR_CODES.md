# Error Codes Reference

All gateway error responses follow a consistent JSON format with machine-readable error codes. These codes form a **stable API contract** — clients can program against them for automated error handling.

## Response Format

```json
{
  "error": "Not Found",
  "error_code": "GATEWAY_ROUTE_NOT_FOUND",
  "message": "no matching route",
  "request_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `error` | string | HTTP status text (e.g. "Not Found", "Unauthorized") |
| `error_code` | string | Stable machine-readable code (see table below) |
| `message` | string | Human-readable description of the error |
| `request_id` | string | Request correlation ID (present when `X-Request-ID` header is set) |

## Error Code Catalog

### Routing Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `GATEWAY_ROUTE_NOT_FOUND` | 404 | No configured route matches the request path |
| `GATEWAY_METHOD_NOT_ALLOWED` | 405 | Route exists but the HTTP method is not in its allowed methods list |

### Upstream Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `GATEWAY_UPSTREAM_UNAVAILABLE` | 502 | Backend service is unreachable or returned an error after all retries |
| `GATEWAY_CIRCUIT_OPEN` | 503 | Circuit breaker is open for this backend — requests are being shed to allow recovery |
| `GATEWAY_REQUEST_CANCELLED` | 504 | Request was cancelled (client disconnect or context deadline exceeded during proxying) |

### Authentication Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `GATEWAY_AUTH_MISSING_TOKEN` | 401 | No `Authorization: Bearer <token>` header found on a route that requires auth |
| `GATEWAY_AUTH_INVALID_TOKEN` | 401 | JWT token is malformed, expired, or has an invalid signature |
| `GATEWAY_AUTH_INSUFFICIENT_SCOPE` | 403 | Token is valid but lacks the required scopes for this route |

### Rate Limiting

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `GATEWAY_RATE_LIMIT_EXCEEDED` | 429 | Client has exceeded the allowed request rate; retry after backing off |

### Request Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `GATEWAY_BODY_TOO_LARGE` | 413 | Request body exceeds the configured `max_body_bytes` limit |
| `GATEWAY_DEADLINE_EXCEEDED` | 504 | Request exceeded the global timeout (`global_timeout_ms`) before completing |

### Internal Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `GATEWAY_INTERNAL_ERROR` | 500 | An unexpected panic was recovered; no internal details are exposed to clients |

## Client Usage

Error codes are designed for programmatic error handling:

```go
// Example: retry on rate limit, fail on auth errors
switch resp.ErrorCode {
case "GATEWAY_RATE_LIMIT_EXCEEDED":
    time.Sleep(backoff)
    return retry(req)
case "GATEWAY_AUTH_INVALID_TOKEN":
    return refreshTokenAndRetry(req)
case "GATEWAY_CIRCUIT_OPEN":
    // Backend is unhealthy, try fallback
    return useFallback(req)
}
```

## Security

Internal implementation details (stack traces, upstream error messages, internal IPs) are never included in error responses. The `GATEWAY_INTERNAL_ERROR` response contains only the generic message — check server logs for the full panic trace using the `request_id` for correlation.
