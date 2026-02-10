.PHONY: build test lint run docker docker-up docker-down clean

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

clean:
	rm -rf bin/
