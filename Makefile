VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/adeoluo/vectordb-go/internal/cli.Version=$(VERSION)
PYLIB ?= ../ann-vector-search-library
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all build test race vet fmt fmt-check bench compare release docker clean

all: fmt-check vet race build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/vectordb ./cmd/vectordb

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run make fmt" && exit 1)

bench:
	go test -run '^$$' -bench . -benchmem ./internal/hnsw ./internal/wal

compare: build
	uv run --project $(PYLIB) python benchmarks/compare.py

release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/vectordb_$(VERSION)_$${os}_$${arch} ./cmd/vectordb || exit 1; \
	done
	cd dist && shasum -a 256 vectordb_* > SHA256SUMS

docker:
	docker build -t vectordb-go:$(VERSION) .

clean:
	rm -rf bin dist
