# Hoptrail Makefile.
#
# Three escalating levels of "install":
#
#   make build      - builds ./hoptrail in the project root, no sudo
#                     Use for: running tests, validating compiles, CI.
#
#   make install    - build + setcap cap_net_raw+ep on ./hoptrail
#                     Use for: running the daemon locally during dev
#                     (`./hoptrail serve --config ./config.yaml.example`).
#                     Sudo prompt for setcap; nothing leaves the project dir.
#
#   make deploy     - runs ./install.sh, deploying to /opt/hoptrail/
#                     with a systemd unit, data dir, the works.
#                     Use for: actually running this as a managed service.
#                     Sudo prompts as the script needs them.
#
# Targets for hot-reload UI development:
#
#   make web-dev    - run Vite dev server (UI on :5173 with HMR)
#   make go-dev     - run the daemon directly (API on :8080, no setcap)
#                     Use in two terminals; Vite proxies /api to :8080.
#
# Why build and install are separate: every `go build` produces a new
# inode, which strips the cap_net_raw+ep file capability — caps are
# an inode-level attribute, not a path-level one. So during pure-Go
# iteration (writing tests, refactoring) you don't want sudo every time.
# When you actually want to run the daemon, `make install` does both.
#
# Why install.sh exists separately: deploying to /opt/hoptrail/ with a
# systemd unit is a bigger ceremony than dev iteration needs. The
# Makefile handles the inner loop; install.sh handles the deployment.

BINARY ?= ./hoptrail
CONFIG ?= ./config.yaml.example

# VERSION is injected into the binary at link time. `git describe` produces
# `<latest-tag>-<n>-g<sha>[-dirty]` — exact tag if HEAD is tagged, otherwise
# the closest tag plus how many commits / which SHA, plus `-dirty` if the
# tree has uncommitted changes. Falls back to `dev` for builds outside a
# git checkout (e.g. extracted source tarball).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -ldflags "-X main.version=$(VERSION)"

.PHONY: web tidy build install deploy uninstall web-dev go-dev test race clean help

help:
	@echo "hoptrail — make targets"
	@echo ""
	@echo "  Build & local install:"
	@echo "    make build      build ./hoptrail (no sudo)"
	@echo "    make install    build + setcap cap_net_raw+ep on ./hoptrail"
	@echo "    make deploy     run ./install.sh — full systemd deployment to /opt/hoptrail/"
	@echo "    make uninstall  run ./uninstall.sh"
	@echo ""
	@echo "  Dev iteration with hot-reload:"
	@echo "    make web-dev    Vite dev server on :5173 (proxies /api → :8080)"
	@echo "    make go-dev     run the daemon on :8080 (no setcap — needs sudo or run as root)"
	@echo ""
	@echo "  Tests:"
	@echo "    make test       go test ./..."
	@echo "    make race       go test -race ./..."
	@echo ""
	@echo "  Maintenance:"
	@echo "    make tidy       go mod tidy (run automatically before build)"
	@echo "    make clean      remove build artifacts (keeps dist placeholder)"

# go mod tidy as a prerequisite of build — this kills the recurring
# "missing go.sum entry" failure on a fresh extract. Cheap to run on
# every build; idempotent when nothing needs tidying.
tidy:
	@go mod tidy

web: web/node_modules
	cd web && npm run build

# Auto-install web dependencies if node_modules/ is missing. This is a
# directory target — make sees the dir's mtime and only runs the recipe
# when the directory doesn't exist. Idempotent and cheap; eliminates
# the "vite: not found" failure on the first build after a fresh extract.
web/node_modules:
	cd web && npm install

build: tidy web
	go build $(LDFLAGS) -o $(BINARY) ./cmd/hoptrail
	@echo
	@echo "built $(BINARY)"
	@echo "next: 'make install' (setcap), 'make deploy' (systemd), or '$(BINARY) serve --config $(CONFIG)'"

install: build
	sudo setcap cap_net_raw+ep $(BINARY)
	@echo "applied cap_net_raw+ep to $(BINARY)"
	@echo "run with: $(BINARY) serve --config $(CONFIG)"

deploy: build
	./install.sh

uninstall:
	./uninstall.sh

web-dev:
	cd web && npm run dev

go-dev:
	go run ./cmd/hoptrail serve --config $(CONFIG) --stream

test: tidy
	go test ./...

race: tidy
	go test -race ./...

clean:
	rm -rf internal/web/dist/assets web/node_modules/.vite $(BINARY)
	@echo "kept internal/web/dist/index.html (placeholder for //go:embed)"
