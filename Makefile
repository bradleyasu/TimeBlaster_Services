# Timeblaster development and deployment tasks.
#
# Everything here runs on a development machine (macOS or Linux) with nothing
# but the Go toolchain: the hardware, mpv, ErsatzTV and the sound card all sit
# behind interfaces with fakes, so `make test` needs no Raspberry Pi.
#
# Run `make help` for the full list.

SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE   := github.com/bradsheets/timeblaster
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS  := -trimpath

# setup.sh installs Go under /usr/local/go, which Raspberry Pi OS puts on no
# shell's PATH -- not even a login one. Falling back to the explicit path is
# what lets `make update` build on the Pi without the caller arranging a PATH.
GO       ?= $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)
GOFMT    ?= $(dir $(GO))gofmt

BIN_DIR  := bin
CMDS     := timeblasterd timeblaster-wifi tbctl

# The Pi's architecture. Every dependency is pure Go, so cross-compiling needs
# no C toolchain and no cgo.
PI_GOOS   := linux
PI_GOARCH := arm64

# Override to deploy somewhere else: make deploy PI=bedroom.local PI_USER=pi
#
# PI_KEY names an SSH identity when the Pi does not use your default key:
#   make deploy PI_USER=joctv PI_KEY=~/.ssh/timeblaster
#
# Targets that need root on the Pi run over `ssh -t`, so sudo can prompt for a
# password. Passwordless sudo is not assumed.
PI       ?= timeblaster.local
PI_USER  ?= $(shell whoami)
PI_KEY   ?=
# Where the repository is checked out on the Pi, for `make deploy-git`.
PI_CHECKOUT ?= ~/timeblaster

# The branch deploys are checked against.
GIT_BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
SSH_OPTS := $(if $(PI_KEY),-i $(PI_KEY),)
PI_SSH   := $(PI_USER)@$(PI)

.PHONY: help
help: ## Show this help
	@printf '\nTimeblaster — make targets\n\n'
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@printf '\nVersion: $(VERSION)\n\n'

# --- Build ------------------------------------------------------------------

.PHONY: build
build: ## Build all binaries for this machine
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		printf '  building %s\n' "$$cmd"; \
		CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" \
			-o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit 1; \
	done
	@printf '\nBuilt $(words $(CMDS)) binaries in $(BIN_DIR)/\n'

.PHONY: build-pi
build-pi: ## Cross-compile for the Raspberry Pi (linux/arm64)
	@mkdir -p $(BIN_DIR)/$(PI_GOOS)-$(PI_GOARCH)
	@for cmd in $(CMDS); do \
		printf '  building %s for $(PI_GOOS)/$(PI_GOARCH)\n' "$$cmd"; \
		GOOS=$(PI_GOOS) GOARCH=$(PI_GOARCH) CGO_ENABLED=0 \
			$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" \
			-o $(BIN_DIR)/$(PI_GOOS)-$(PI_GOARCH)/$$cmd ./cmd/$$cmd || exit 1; \
	done
	@printf '\nBuilt for the Pi in $(BIN_DIR)/$(PI_GOOS)-$(PI_GOARCH)/\n'

.PHONY: assets
assets: ## Regenerate the PWA icons and the television screens
	python3 scripts/make-assets.py

.PHONY: splash-check
splash-check: ## Report what the boot screen would paint (safe anywhere)
	python3 scripts/timeblaster-splash --check

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) coverage.out coverage.html
	$(GO) clean -testcache

# --- Test -------------------------------------------------------------------

.PHONY: test
test: ## Run all Go tests with the race detector
	$(GO) test -race -timeout 300s ./...

.PHONY: firmware-test
firmware-test: ## Run the Arduino firmware's host tests (no board required)
	@$(MAKE) --no-print-directory -C firmware/timeblaster-nano test

.PHONY: firmware-fixture
firmware-fixture: ## Regenerate the protocol fixture from the firmware encoder
	@$(MAKE) --no-print-directory -C firmware/timeblaster-nano fixture

.PHONY: test-short
test-short: ## Run tests without the race detector (faster)
	$(GO) test -timeout 120s ./...

.PHONY: cover
cover: ## Run tests and open a coverage report
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1
	$(GO) tool cover -html=coverage.out -o coverage.html
	@printf '\nCoverage report: coverage.html\n'

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test -bench=. -benchmem -run='^$$' ./...

# --- Quality ----------------------------------------------------------------

.PHONY: lint
lint: fmt-check vet ## Run all static checks
	@if command -v staticcheck >/dev/null 2>&1; then \
		printf '  staticcheck\n'; staticcheck ./...; \
	else \
		printf '  staticcheck not installed; skipping\n'; \
		printf '    go install honnef.co/go/tools/cmd/staticcheck@latest\n'; \
	fi
	@if command -v shellcheck >/dev/null 2>&1; then \
		printf '  shellcheck\n'; shellcheck -S warning setup.sh; \
	else \
		printf '  shellcheck not installed; skipping setup.sh\n'; \
	fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	$(GOFMT) -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is unformatted
	@unformatted=$$($(GOFMT) -s -l . | grep -v '^$$' || true); \
	if [ -n "$$unformatted" ]; then \
		printf 'These files need gofmt:\n%s\n' "$$unformatted"; exit 1; \
	fi
	@printf '  formatting ok\n'

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	$(GO) mod tidy
	$(GO) mod verify

.PHONY: check
check: lint test firmware-test ## Everything CI would run

# --- Run locally ------------------------------------------------------------

.PHONY: dev
dev: ## Run timeblasterd locally against a scratch config (no hardware needed)
	@mkdir -p .dev/run .dev/sounds
	@if [ ! -f .dev/timeblaster.toml ]; then \
		sed -e 's|/var/lib/timeblaster/timeblaster.db|.dev/timeblaster.db|' \
		    -e 's|/var/lib/timeblaster/alarm-sounds|.dev/sounds|' \
		    -e 's|/run/timeblaster|.dev/run|g' \
		    -e 's|/usr/share/timeblaster/assets/no-channel.png|deploy/assets/no-channel.png|' \
		    deploy/config/timeblaster.toml > .dev/timeblaster.toml; \
		printf 'Created .dev/timeblaster.toml\n'; \
	fi
	$(GO) run ./cmd/timeblasterd --config .dev/timeblaster.toml --log-level debug --log-time

.PHONY: check-config
check-config: ## Validate the shipped configuration file
	$(GO) run ./cmd/timeblasterd --config deploy/config/timeblaster.toml --check-config

.PHONY: print-config
print-config: ## Print the effective default configuration
	$(GO) run ./cmd/timeblasterd --print-config

# --- Install and operate on the Pi ------------------------------------------

.PHONY: install
install: ## Install on THIS machine (run on the Pi, as root)
	@if [ "$$(id -u)" != "0" ]; then printf 'Run as root: sudo make install\n'; exit 1; fi
	./setup.sh

.PHONY: deploy
deploy: verify-synced build-pi ## Cross-compile and copy the binaries to the Pi over SSH
	@printf 'Deploying $(VERSION) to $(PI_SSH)\n'
	scp $(SSH_OPTS) $(BIN_DIR)/$(PI_GOOS)-$(PI_GOARCH)/* $(PI_SSH):/tmp/
	ssh -t $(SSH_OPTS) $(PI_SSH) 'sudo install -m 0755 /tmp/timeblasterd /tmp/timeblaster-wifi /tmp/tbctl /usr/local/bin/ && \
	           rm -f /tmp/timeblasterd /tmp/timeblaster-wifi /tmp/tbctl && \
	           sudo systemctl restart timeblaster-wifi.service timeblaster.service'
	@printf 'Deployed. Check with: make logs\n'

.PHONY: verify-synced
verify-synced: ## Fail unless the tree is clean and matches origin
# `make deploy` cross-compiles the working tree, so without this a deployed
# binary can contain changes that exist on no other machine and in no commit --
# with the "-dirty" suffix in the version string as the only clue, and only if
# someone thinks to look. Set ALLOW_DIRTY=1 to deploy work in progress on
# purpose.
ifdef ALLOW_DIRTY
	@printf '\nALLOW_DIRTY is set: deploying the working tree.\n'
	@printf 'What ends up on the Pi may exist nowhere else. Version: $(VERSION)\n\n'
else
	@if [ -n "$$(git status --porcelain)" ]; then 		printf '\nRefusing to deploy: the working tree has uncommitted changes.\n\n'; 		git status --short; 		printf '\nCommit and push, or deploy anyway with ALLOW_DIRTY=1.\n\n'; 		exit 1; 	fi
	@git fetch -q origin $(GIT_BRANCH) 2>/dev/null || { 		printf '\nRefusing to deploy: could not reach origin to compare against.\n'; 		printf 'Deploy anyway with ALLOW_DIRTY=1.\n\n'; 		exit 1; 	}
	@head=$$(git rev-parse HEAD); 	remote=$$(git rev-parse origin/$(GIT_BRANCH) 2>/dev/null); 	if [ "$$head" != "$$remote" ]; then 		printf '\nRefusing to deploy: HEAD is not what origin/%s has.\n\n' '$(GIT_BRANCH)'; 		printf '  HEAD           %s\n' "$$(git log --oneline -1 HEAD)"; 		printf '  origin/%s  %s\n\n' '$(GIT_BRANCH)' "$$(git log --oneline -1 origin/$(GIT_BRANCH))"; 		printf 'Push first, or deploy anyway with ALLOW_DIRTY=1.\n\n'; 		exit 1; 	fi
	@printf 'In sync with origin/%s at %s\n' '$(GIT_BRANCH)' "$$(git rev-parse --short HEAD)"
endif

.PHONY: update
update: ## Pull and deploy on THIS machine (run on the Pi)
	@if [ ! -d .git ]; then printf 'Not a git checkout.\n'; exit 1; fi
	@printf 'Updating from %s\n' "$$(git remote get-url origin)"
	@if [ -n "$$(git status --porcelain)" ]; then 		printf '\nThe working tree has local changes. Commit or discard them first:\n\n'; 		git status --short; 		printf '\n'; exit 1; 	fi
	git pull --ff-only
	@$(MAKE) --no-print-directory build
	sudo install -m 0755 $(BIN_DIR)/timeblasterd $(BIN_DIR)/timeblaster-wifi $(BIN_DIR)/tbctl /usr/local/bin/
	sudo systemctl restart timeblaster-wifi.service timeblaster.service
	@sleep 4
	@printf '\nNow running %s\n' "$$(tbctl version 2>/dev/null || echo '?')"
	@systemctl is-active timeblaster.service timeblaster-wifi.service

.PHONY: deploy-git
deploy-git: ## Tell the Pi to pull and deploy itself (run from a dev machine)
	@printf 'Asking %s to update from git\n' "$(PI_SSH)"
	ssh -t $(SSH_OPTS) $(PI_SSH) 'cd $(PI_CHECKOUT) && make update'

.PHONY: restart
restart: ## Restart the services on the Pi
	ssh -t $(SSH_OPTS) $(PI_SSH) 'sudo systemctl restart timeblaster-wifi.service timeblaster.service'
	@ssh $(SSH_OPTS) $(PI_SSH) 'systemctl is-active timeblaster.service timeblaster-wifi.service' || true

.PHONY: stop
stop: ## Stop the services on the Pi
	ssh -t $(SSH_OPTS) $(PI_SSH) 'sudo systemctl stop timeblaster.service timeblaster-wifi.service'

.PHONY: status
status: ## Show service status on the Pi
	ssh $(SSH_OPTS) $(PI_SSH) 'systemctl status --no-pager timeblaster.service timeblaster-wifi.service ersatztv.service' || true

.PHONY: logs
logs: ## Follow the daemon log on the Pi
	ssh $(SSH_OPTS) $(PI_SSH) 'journalctl -u timeblaster.service -f --no-pager'

.PHONY: logs-all
logs-all: ## Follow every Timeblaster-related log on the Pi
	ssh $(SSH_OPTS) $(PI_SSH) 'journalctl -u timeblaster.service -u timeblaster-wifi.service -u ersatztv.service -f --no-pager'

.PHONY: logs-debug
logs-debug: ## Restart the daemon at debug level and follow its log
	ssh -t $(SSH_OPTS) $(PI_SSH) 'sudo systemctl stop timeblaster.service && \
	           sudo -u timeblaster /usr/local/bin/timeblasterd \
	             --config /etc/timeblaster/timeblaster.toml --log-level debug --log-time'

.PHONY: health
health: ## Query the health endpoint on the Pi
	ssh $(SSH_OPTS) $(PI_SSH) 'tbctl health'

# --- Firmware ---------------------------------------------------------------

.PHONY: firmware
firmware: ## Print how to build and upload the Nano firmware
	@printf '\nThe Nano firmware is a PlatformIO project:\n'
	@printf '  firmware/timeblaster-nano/\n\n'
	@printf 'Build and flash:\n'
	@printf '  cd firmware/timeblaster-nano\n'
	@printf '  pio run                 # build\n'
	@printf '  pio run -t upload       # flash\n'
	@printf '  pio device monitor      # watch the TB1 link\n\n'
	@printf 'Stop the daemon first; the serial port takes one reader:\n'
	@printf '  sudo systemctl stop timeblaster.service\n\n'
	@printf 'Host tests, no board required:\n'
	@printf '  make firmware-test\n\n'
	@printf 'See docs/hardware.md for wiring and firmware/timeblaster-nano/README.md\n'
	@printf 'for why the display driver must not be edited in place.\n\n'
