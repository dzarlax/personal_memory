.PHONY: help dev-deps build test vet tidy clean

DS_REF ?= main
DS_BASE := https://cdn.jsdelivr.net/gh/dzarlax/design-system@$(DS_REF)/dist
DS_DIR  := internal/viz/static/assets/vendor

help:
	@echo "Targets:"
	@echo "  make dev-deps  — fetch the dzarlax design-system bundle into $(DS_DIR)"
	@echo "  make build     — go build both binaries (runs dev-deps first)"
	@echo "  make test      — go vet + go test"
	@echo "  make clean     — remove built binaries and the vendored DS bundle"

dev-deps:
	@mkdir -p $(DS_DIR)
	@echo "Fetching design system from $(DS_BASE) ..."
	@curl -fsSL "$(DS_BASE)/dzarlax.css" -o $(DS_DIR)/dzarlax.css
	@curl -fsSL "$(DS_BASE)/dzarlax.js"  -o $(DS_DIR)/dzarlax.js
	@echo "OK — bundle at $(DS_DIR)/"

build: dev-deps
	go build ./cmd/server ./cmd/indexer

vet:
	go vet ./...

test: vet
	go test ./...

tidy:
	go mod tidy

clean:
	rm -rf $(DS_DIR) /tmp/personal-memory /tmp/personal-memory-indexer
