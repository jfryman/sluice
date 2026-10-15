# Sluice build + iteration targets. See lode/development.md.
#
#   make            fmt-check, vet, test, build
#   make dev        TUI against the full sandbox clone (sluice -sandbox)
#   make watch      rebuild + test on every .go change
#   make run        TUI against real mail and real sieve config

BIN      := bin/sluice
PKG      := ./cmd/sluice
GOFILES  := $(shell find . -name '*.go')

# Build metadata from git; see lode/development.md#versioning.
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null)
BRANCH   := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
DATE     := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VPKG     := github.com/jfryman/sluice/internal/version
LDFLAGS  := -X $(VPKG).Version=$(VERSION) -X $(VPKG).Commit=$(COMMIT) \
            -X $(VPKG).Branch=$(BRANCH) -X $(VPKG).Date=$(DATE)

.PHONY: pkg-check-pushed
.PHONY: all build test vet fmt fmt-check check run scan dev dev-scan dev-reset watch install uninstall clean version $(BIN) \
        install-service uninstall-service service-logs package install-package

all: check build

build: $(BIN)

# Always relinks (go build is incremental) so a new commit or tag is stamped.
$(BIN):
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

version:
	@echo "$(VERSION) (commit $(COMMIT), branch $(BRANCH))"

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

# Removes what install / install-service put in place (binary + user unit).
# Config, plan, journal, index and sandbox are left alone.
uninstall: uninstall-service
	rm -f $(HOME)/.local/bin/sluice

# --- sweep service (systemd user unit) on the machine this is run on ---------

UNIT_DIR := $(HOME)/.config/systemd/user
UNIT     := sluice-sweep.service

install-service: install
	install -Dm 0644 contrib/systemd/$(UNIT) $(UNIT_DIR)/$(UNIT)
	systemctl --user daemon-reload
	systemctl --user enable --now $(UNIT)
	systemctl --user restart $(UNIT)   # pick up a newly installed binary

# Safe to re-run: skips disabling when the unit isn't installed.
uninstall-service:
	@if [ -e $(UNIT_DIR)/$(UNIT) ]; then \
		echo "systemctl --user disable --now $(UNIT)"; \
		systemctl --user disable --now $(UNIT) || true; \
	fi
	rm -f $(UNIT_DIR)/$(UNIT)
	systemctl --user daemon-reload

service-logs:
	journalctl --user -u $(UNIT) -f

# --- Arch package (contrib/arch); see lode/development.md#packaging -------------
# makepkg runs on a copy under bin/pkg so it never rewrites contrib/arch/PKGBUILD's
# pkgver line. The package builds GitHub main, so unpushed commits aren't in it.

PKGDIR        := bin/pkg
MAKEPKG_FLAGS ?= -s

$(PKGDIR)/PKGBUILD: contrib/arch/PKGBUILD contrib/arch/sluice.install
	@mkdir -p $(PKGDIR)
	cp contrib/arch/PKGBUILD contrib/arch/sluice.install $(PKGDIR)/

pkg-check-pushed:
	@n=$$(git rev-list --count @{u}..HEAD 2>/dev/null || echo 0); \
	if [ "$$n" != 0 ]; then echo "warning: $$n local commit(s) not pushed; the package builds GitHub main without them"; fi

package: pkg-check-pushed $(PKGDIR)/PKGBUILD
	cd $(PKGDIR) && makepkg -fc $(MAKEPKG_FLAGS)

install-package: pkg-check-pushed $(PKGDIR)/PKGBUILD
	cd $(PKGDIR) && makepkg -fci $(MAKEPKG_FLAGS)

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
	-chmod -R u+w bin 2>/dev/null
	rm -rf bin
