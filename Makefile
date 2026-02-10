.PHONY: build test lint run docker docker-up docker-down clean test-integration test-fuzz test-load

BINARY=gateway
ECHOSERVER=echoserver

build:
	go build -o bin/$(BINARY) ./cmd/gateway
	go build -o bin/$(ECHOSERVER) ./cmd/echoserver

test:
	go test ./... -v -cover -race

lint:
	go vet ./...
	staticcheck ./...

run: build
	JWT_SECRET=$${JWT_SECRET:-$$(openssl rand -base64 32)} ./bin/$(BINARY) -config configs/gateway.yaml

docker:
	docker build -t gateway-core .

docker-up:
	docker-compose up --build -d

docker-down:
	docker-compose down

test-integration:
	@echo "Starting integration test stack..."
	@echo "JWT_SECRET=integration-test-secret-key-32chars!!" > .env
	docker compose -f docker-compose.yml -f tests/integration/docker-compose.integration.yaml up --build -d
	@echo "Waiting for gateway..."
	@for i in $$(seq 1 60); do \
		curl -sf http://localhost:8080/health > /dev/null 2>&1 && break; \
		sleep 1; \
	done
	go test -tags integration -v -timeout 120s ./tests/integration/
	docker compose -f docker-compose.yml -f tests/integration/docker-compose.integration.yaml down -v

test-fuzz:
	go test ./internal/config -fuzz=FuzzLoadFromBytes -fuzztime=30s
	go test ./internal/routing -fuzz=FuzzMatchesPrefix -fuzztime=30s
	go test ./internal/auth -fuzz=FuzzAuthMiddleware -fuzztime=30s

test-load:
	@echo "Ensure gateway stack is running (make docker-up first)"
	JWT_SECRET=$${JWT_SECRET:-integration-test-secret-key-32chars!!} go run tests/load/gen-token.go > /tmp/jwt_token.txt
	k6 run --env GATEWAY_URL=http://localhost:8080 --env JWT_TOKEN=$$(cat /tmp/jwt_token.txt) tests/load/k6-baseline.js
	@rm -f /tmp/jwt_token.txt

clean:
	rm -rf bin/
