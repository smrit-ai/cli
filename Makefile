SERVER ?= http://localhost:8000
MODULE := github.com/smrit-ai/cli

.PHONY: test build dev

test:
	go test ./...
	go vet ./...

build:
	go build -o bin/smrit ./cmd/smrit

dev:
	go build -tags dev -ldflags "-X $(MODULE)/internal/server.Address=$(SERVER)" -o bin/smrit ./cmd/smrit
