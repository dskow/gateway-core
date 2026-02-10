# go-api-gateway

A lightweight, config-driven API gateway written in Go that provides JWT/OAuth2 authentication, per-client rate limiting, and reverse proxy routing to multiple backend services.

## Architecture

```mermaid
graph TD
    Client[Client] --> GW[API Gateway :8080]

    subgraph Gateway Middleware Stack
        GW --> Recovery[Recovery]
        Recovery --> Logging[Structured Logging]
        Logging --> CORS[CORS]
        CORS --> RateLimit[Rate Limiter]
        RateLimit --> Auth[JWT Auth]
        Auth --> Proxy[Reverse Proxy Router]
    end

    Proxy -->|/api/users/*| US[Users Service :3001]
    Proxy -->|/api/analytics/*| AS[Analytics Service :3002]
    Proxy -->|/webhooks/*| WH[Webhook Handler :3003]

    GW --> Health[/health /ready]
```

## Features

- **Config-driven routing** — Define routes, backends, and behavior in YAML
- **JWT/OAuth2 authentication** — HMAC-SHA256 token validation with issuer, audience, and scope checks
- **Per-client rate limiting** — Token bucket algorithm with per-route overrides
- **Reverse proxy** — Path prefix matching, prefix stripping, header injection, retries with backoff
- **Structured logging** — JSON request logs with method, path, status, latency, and request ID
- **Health checks** — Liveness (`/health`) and readiness (`/ready`) endpoints
- **Graceful shutdown** — Drains in-flight requests on SIGINT/SIGTERM
- **CORS support** — Configurable allowed origins, methods, and headers
- **Panic recovery** — Catches panics and returns structured error responses

## Quickstart with Docker Compose

```bash
# 1. Create a .env file with your JWT secret (git-ignored)
cp .env.example .env        # then edit .env with a strong secret

# 2. Start the gateway and backend services
docker compose up --build
```

This starts the gateway on port 8080 with two echo backend services. Test it:

```bash
# Public route (no auth required)
curl http://localhost:8080/public/hello

# Generate a JWT for authenticated routes
# Using https://jwt.io or a script (replace <your-secret> with the value in .env):
export TOKEN=$(python3 -c "
import jwt, time
print(jwt.encode({
    'sub': 'user-1',
    'iss': 'https://auth.example.com',
    'aud': 'api-gateway',
    'exp': int(time.time()) + 3600,
    'scope': 'read write'
}, '<your-jwt-secret>', algorithm='HS256'))
")

# Authenticated route
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/users/123

# Rate limiting — send many requests quickly
for i in $(seq 1 60); do
  curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/public/test
done

# Health check
curl http://localhost:8080/health

# Readiness check
curl http://localhost:8080/ready
```

## Manual Build & Run

**Prerequisites:** Go 1.22+

```bash
# Build
make build

# Run tests
make test

# Run locally (starts on :8080; generates a random secret if JWT_SECRET is unset)
make run
# Or supply your own:
JWT_SECRET=my-secret make run

# Lint
make lint
```

## Configuration Reference

### Server

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `server.port` | int | `8080` | Listen port |
| `server.read_timeout` | duration | `15s` | HTTP read timeout |
| `server.write_timeout` | duration | `15s` | HTTP write timeout |
| `server.shutdown_timeout` | duration | `10s` | Graceful shutdown timeout |

### Rate Limiting

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `rate_limit.requests_per_second` | float | `100` | Global requests per second per client |
| `rate_limit.burst_size` | int | `50` | Maximum burst size per client |

### Authentication

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.enabled` | bool | `false` | Enable JWT validation |
| `auth.jwt_secret` | string | — | HMAC-SHA256 signing secret (supports `${ENV_VAR}`) |
| `auth.issuer` | string | — | Expected JWT issuer |
| `auth.audience` | string | — | Expected JWT audience |
| `auth.scopes` | []string | `[]` | Required OAuth2 scopes |

### Routes

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `routes[].path_prefix` | string | — | URL path prefix to match (required) |
| `routes[].backend` | string | — | Backend service URL (required) |
| `routes[].strip_prefix` | bool | `false` | Strip the path prefix before forwarding |
| `routes[].methods` | []string | all | Allowed HTTP methods |
| `routes[].auth_required` | bool | `false` | Require JWT authentication |
| `routes[].timeout_ms` | int | `30000` | Request timeout in milliseconds |
| `routes[].retry_attempts` | int | `0` | Retry attempts on 502/503/504 |
| `routes[].headers` | map | — | Custom headers to inject |
| `routes[].rate_override` | object | — | Per-route rate limit override |

## Example curl Commands

```bash
# Basic routing
curl http://localhost:8080/api/users/123

# Auth with JWT
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/users

# See rate limiting in action (429 after burst)
for i in $(seq 1 100); do
  curl -s -o /dev/null -w "%{http_code} " http://localhost:8080/api/users
done

# Health check
curl http://localhost:8080/health
# {"status":"ok"}

# Readiness probe
curl http://localhost:8080/ready
# {"status":"ready","backends":{"/api/users":"ok","/api/analytics":"ok"}}
```

## Project Structure

```
cmd/gateway/main.go          — Entry point, signal handling, graceful shutdown
cmd/echoserver/main.go       — Simple echo backend for testing
internal/
  config/config.go           — YAML config loader with validation and defaults
  auth/auth.go               — JWT/OAuth2 Bearer token validation middleware
  ratelimit/ratelimit.go     — Per-client-IP token bucket rate limiter middleware
  proxy/proxy.go             — Reverse proxy with route matching, retries, timeouts
  middleware/logging.go       — Structured JSON request/response logging
  middleware/cors.go          — CORS middleware
  middleware/recovery.go      — Panic recovery middleware
  health/health.go            — Health check and readiness endpoints
configs/
  gateway.yaml                — Example configuration file
  docker-gateway.yaml         — Docker Compose configuration
Dockerfile                    — Multi-stage build for minimal production image
docker-compose.yml            — Gateway + 2 sample backends for demo
Makefile                      — build, test, lint, run, docker targets
```

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/golang-jwt/jwt/v5` | JWT parsing and validation |
| `golang.org/x/time` | Token bucket rate limiter |
| `gopkg.in/yaml.v3` | YAML configuration parsing |
| Go stdlib | Everything else (HTTP, logging, crypto, etc.) |

## Why This Project

This project demonstrates backend engineering patterns commonly used in integration platforms — the kind of systems that connect with third-party services like Salesforce, Slack, and Segment:

- **Request routing** — Directing API calls to the correct backend service based on path matching
- **Authentication** — Validating OAuth2/JWT tokens before forwarding requests
- **Rate limiting** — Protecting upstream services from overload with per-client limits
- **Reliable proxying** — Retries with exponential backoff, timeouts, and structured error handling
- **Observability** — Structured logging with request IDs for distributed tracing
- **Configuration** — Clean, declarative configuration with environment variable support
