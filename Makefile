VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: run test cover bench lint vuln build up down reload

run:
	GATEWAY_API_KEY=dev-key go run ./cmd/gateway -config configs/gateway.json

test:
	go test -race ./...

cover:
	go test -coverprofile=cover.out ./... && go tool cover -html=cover.out

bench:
	go test -run '^$$' -bench . -benchmem ./...

lint:
	test -z "$$(gofmt -l .)" && go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

build:
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o bin/gateway ./cmd/gateway

up:
	docker compose up --build -d

down:
	docker compose down

reload:
	docker compose kill -s HUP gateway
