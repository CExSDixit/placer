BINARY := adbfz
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

# Every target builds with CGO_ENABLED=0. This is a hard constraint, not a
# preference: it is what keeps adbfz a single static binary that cross-compiles
# with no C toolchain. Any dependency that breaks it is rejected.
export CGO_ENABLED=0

.PHONY: all build test race vet fmt check release clean run fixtures

all: check build

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test race
	@test -z "$$(gofmt -l . )" || (echo "unformatted files:"; gofmt -l .; exit 1)

# Proves the single-binary story on every supported target.
release: check
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -ldflags="$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch . || exit 1; \
	done
	@ls -lh dist/

# Run against synthetic data — no device needed.
run:
	go run . -fake

# Record real device output for offline development.
fixtures:
	go run ./cmd/capture-fixtures -out testdata/fixtures

clean:
	rm -rf $(BINARY) dist/
