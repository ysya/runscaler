BINARY_NAME := runscaler
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE  := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS     := -s -w
GOFLAGS     := -trimpath

PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64

.PHONY: build clean test all dev release

## dev: Run locally with debug logging (requires config.toml)
dev:
	go run ./cmd/runscaler --log-level debug

## build: Build for current platform
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/runscaler

## test: Run tests
test:
	go test -v ./...

## all: Build for all platforms into dist/
all: clean $(PLATFORMS)

$(PLATFORMS):
	$(eval OS := $(word 1,$(subst /, ,$@)))
	$(eval ARCH := $(word 2,$(subst /, ,$@)))
	$(eval EXT := $(if $(filter windows,$(OS)),.exe,))
	@echo "Building $(OS)/$(ARCH)..."
	@mkdir -p dist
	GOOS=$(OS) GOARCH=$(ARCH) go build \
		$(GOFLAGS) -ldflags "$(LDFLAGS)" \
		-o dist/$(BINARY_NAME)-$(OS)-$(ARCH)$(EXT) ./cmd/runscaler

## clean: Remove build artifacts
clean:
	rm -rf dist/ $(BINARY_NAME)

## release: Interactive release — pick version, tag, and push
release:
	@sh scripts/release.sh

## help: Show this help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':'
