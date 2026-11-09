SERVER ?= http://localhost:8000
MODULE := github.com/smrit-ai/cli
TARGETS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: test test-install build dev release

test:
	go test ./...
	go vet ./...

test-install:
	sh scripts/install_test.sh

build:
	go build -o bin/smrit ./cmd/smrit

dev:
	go build -tags dev -ldflags "-X $(MODULE)/internal/server.Address=$(SERVER)" -o bin/smrit ./cmd/smrit

# make release VERSION=x.y.z writes the four tarballs, SHA256SUMS and install.sh to dist/.
release:
	@test -n "$(VERSION)" || { echo "Set VERSION, for example: make release VERSION=0.1.0"; exit 1; }
	mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		stage=$$(mktemp -d); \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X $(MODULE)/internal/version.Version=$(VERSION)" \
			-o $$stage/smrit ./cmd/smrit || exit 1; \
		tar -czf dist/smrit_$${os}_$${arch}.tar.gz -C $$stage smrit; \
	done
	cd dist && { command -v sha256sum >/dev/null && sha256sum smrit_*.tar.gz || shasum -a 256 smrit_*.tar.gz; } > SHA256SUMS
	cp scripts/install.sh dist/install.sh
