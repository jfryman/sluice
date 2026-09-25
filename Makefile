# Sluice build + iteration targets. See lode/development.md.
#
#   make            fmt-check, vet, test, build
#   make dev        TUI against the full sandbox clone (sluice -sandbox)
#   make watch      rebuild + test on every .go change
#   make run        TUI against real mail and real sieve config

BIN      := bin/sluice
PKG      := ./cmd/sluice
GOFILES  := $(shell find . -name '*.go')

.PHONY: all build test vet fmt fmt-check check run scan dev dev-scan dev-reset watch install clean

all: check build

build: $(BIN)

$(BIN): $(GOFILES) go.mod go.sum
	@mkdir -p bin
	go build -o $(BIN) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w $(GOFILES)

fmt-check:
	@out=$$(gofmt -l $(GOFILES)); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt-check vet test

# --- real environment -------------------------------------------------------

run: build
	$(BIN)

scan: build
	$(BIN) -scan

install: check build
	install -m 0755 $(BIN) $(HOME)/.local/bin/sluice

# --- sandbox: full reflink clone of real mail; see lode/apply/sandbox.md ----

dev: build
	$(BIN) -sandbox

dev-scan: build
	$(BIN) -sandbox -scan

dev-reset: build
	$(BIN) -reset-sandbox -scan

watch:
	scripts/watch.sh

clean:
	rm -rf bin
