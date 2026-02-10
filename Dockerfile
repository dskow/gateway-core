# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gateway ./cmd/gateway
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /echoserver ./cmd/echoserver

# Production stage
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /gateway /usr/local/bin/gateway
COPY --from=builder /echoserver /usr/local/bin/echoserver
COPY configs/gateway.yaml /etc/gateway/gateway.yaml

EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["gateway"]
CMD ["-config", "/etc/gateway/gateway.yaml"]
