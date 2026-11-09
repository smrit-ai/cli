SERVER ?= http://localhost:8000
TROVE ?= http://localhost:8001
MODULE := github.com/smrit-ai/cli
TARGETS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: test test-install build dev release secret-cases

test:
	go test ./...
	go vet ./...

test-install:
	sh scripts/install_test.sh

# Regenerates testdata/secret_cases.json from the memory plugin's patterns,
# when the plugins repository is checked out beside this one.
secret-cases:
	python3 -I scripts/secret_cases.py

build:
	go build -o bin/smrit ./cmd/smrit

dev:
	go build -tags dev -ldflags "-X $(MODULE)/internal/server.Address=$(SERVER) -X $(MODULE)/internal/server.TroveOrigin=$(TROVE)" -o bin/smrit ./cmd/smrit

# make release VERSION=x.y.z writes the four tarballs, SHA256SUMS and install.sh to dist/.
export VERSION

release:
	@printf '%s' "$$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$$' || { echo "VERSION must look like 1.2.3, for example: make release VERSION=0.1.0"; exit 1; }
	mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		stage=$$(mktemp -d); \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X $(MODULE)/internal/version.Version=$$VERSION" \
			-o $$stage/smrit ./cmd/smrit || exit 1; \
		tar -czf dist/smrit_$${os}_$${arch}.tar.gz -C $$stage smrit; \
	done
	cd dist && { command -v sha256sum >/dev/null && sha256sum smrit_*.tar.gz || shasum -a 256 smrit_*.tar.gz; } > SHA256SUMS
	cp scripts/install.sh dist/install.sh
