# ============================================================
# entity-workbench-go Makefile
# ============================================================
#
# All go invocations go through this Makefile. Never call `go test`
# or `go build` directly — the Makefile pins the toolchain (see
# GOTOOLCHAIN below) and forwards ARGS so callers don't have to
# remember the toolchain or test flags every time.
#
# ---------- Workflow targets ----------
#
#   make test                                  Full test sweep (race, count=1)
#   make test ARGS="-run TestE2E_Mount -v"     Narrow + verbose
#   make test-sdk / test-shell                 Per-module sweeps
#   make test-shellcmd / test-workbench / test-programs
#   make build                                 Build all shipped binaries
#                                              (entity-shell + entity-console)
#   make shell-build                           Just the entity-shell binary
#   make shell                                 go run the shell (REPL)
#   make shell ARGS="info"                     go run the shell (one-shot)
#
# ---------- Generic go alias (escape hatch) ----------
#
# When the workflow targets aren't enough, `make go` proxies any go
# subcommand. Pass everything via ARGS="...", including the
# subcommand itself:
#
#   make go ARGS="test -race -count=1 ./entitysdk/..."
#   make go ARGS="build -ldflags '-X main.version=test' -o /tmp/eshell ./shell/cmd/entity-shell"
#   make go ARGS="run ./shell/cmd/entity-shell help"
#   make go ARGS="vet ./..."
#   make go ARGS="mod tidy"
#   make go ARGS="env GOTOOLCHAIN"
#
# Why ARGS= and not positional passthrough: make treats anything
# starting with `-` as its own flag, so `make go test -race` doesn't
# work — make never sees `-race` as an argument. ARGS= is the clean
# workaround and keeps everything in this Makefile.
#
# ---------- Toolchain ----------
#
# Core-go's ext/go.mod declares `go 1.25.0`; workbench's modules
# declare `go 1.24`. The newer requirement wins under the sibling-
# replace setup, so we need a 1.25.x toolchain. Pinned here once so
# callers never have to set GOTOOLCHAIN themselves. Setting it inline
# on the command line triggers a permission prompt every invocation —
# do NOT do that.
# ============================================================

PARENT := $(shell dirname $(CURDIR))
GOMOD_CACHE := $(HOME)/.cache/go-mod-entity-workbench
GOBUILD_CACHE := $(HOME)/.cache/go-build-entity-workbench

# BIN_DIR is where `make build` installs runnable binaries.
# Default: ~/bin (assumes user has it on PATH). Override per
# invocation, e.g. `make build BIN_DIR=./bin` for a project-local
# build. Binaries are always rebuilt fresh — no incremental cache
# checks beyond what `go build` does internally.
BIN_DIR ?= $(HOME)/bin

# Absolutize BIN_DIR against the repo root. The per-module build targets
# `cd` into their own module before `go build -o $(BIN_DIR)/...`, so a
# RELATIVE BIN_DIR — the `BIN_DIR=bin` the bare-box targets pass, or a
# documented `make build BIN_DIR=./bin` — would otherwise be re-rooted
# inside each module and scatter binaries into console/bin,
# entity-publish/bin, … `override` so a command-line BIN_DIR is caught too.
override BIN_DIR := $(abspath $(BIN_DIR))

export GOTOOLCHAIN ?= go1.25.1

# Podman resource caps (committed defaults + per-machine override). The root
# targets here are all native `go` and run no containers, but caps.mk is the
# single committed source of the CAP_*/PODMAN_*_CAPS standard; avalonia/Makefile
# includes the same file and uses the caps on every podman build/run.
include caps.mk

.PHONY: crossimpl-go workbench-test console-build console-run test test-each test-each-native test-native test-sdk test-shell test-shellboot test-shellcmd test-shellpanel test-workbench test-programs test-inspect test-publish test-fetch perfreview build build-native shell shell-test shell-help shell-once shell-build publish-build publish-serve vcs-build fetch-build go clean clean-strays ensure-bindir image help lint fmt check lint-native lint-perfreview fmt-native

# ============================================================
# make + podman — bare-box entry points
# ============================================================
#
# `make build` / `make test` run the native targets below INSIDE the
# stock pinned golang image (which ships go + make + git), with the
# meta repo bind-mounted at /src/entity-systems so the required sibling
# `../entity-core-go/` resolves. Host needs only `make` + `podman`.
#
# On a machine that already has the Go toolchain, run the `-native`
# targets directly (`make build-native` / `make test-native`) to skip
# the container. To run a SINGLE granular target bare-box, append
# `-box` (e.g. `make test-sdk-box`) — see the `%-box` rule below.
TOOLCHAIN_IMAGE := golang:1.25-bookworm

define IN_CONTAINER
	mkdir -p $(GOMOD_CACHE) $(GOBUILD_CACHE)
	podman run --rm \
		-e GOTOOLCHAIN=local \
		-v $(PARENT):/src/entity-systems:Z \
		-v $(GOMOD_CACHE):/go/pkg/mod:Z \
		-v $(GOBUILD_CACHE):/root/.cache/go-build:Z \
		-w /src/entity-systems/entity-workbench-go \
		$(TOOLCHAIN_IMAGE) \
		$(1)
endef

.DEFAULT_GOAL := help

# ADR-0019 Tier-1 verbs: help build test lint fmt check clean. build/test/lint/fmt
# self-containerize (the `-native` workers run inside the pinned golang image via
# IN_CONTAINER); `make <verb>-native` runs the worker directly on a Go host.
help:
	@echo "entity-workbench-go — make + podman (host needs only make + podman)"
	@echo
	@echo "  START HERE"
	@echo "    make doctor      check prerequisites + sibling kernel; prints what is wrong"
	@echo "    make run         build and start entity-shell (the CLI; REPL)"
	@echo "    make gui         build and launch the Avalonia desktop app"
	@echo "    make gui-run     launch the desktop app WITHOUT rebuilding (fast loop)"
	@echo "    make demo        one-command tour: a peer, a name, a tree, in one shell"
	@echo
	@echo "  BUILD"
	@echo "    make build       every shipped Go binary, in-container -> ./bin"
	@echo "    make gui-build   the Avalonia container image (podman)"
	@echo "    make clean       remove build outputs (canonical binaries + strays)"
	@echo
	@echo "  TEST"
	@echo "    make test-each   EVERY Go suite to completion + a summary table  <- use this"
	@echo "    make test        full -race sweep; STOPS at the first failing package"
	@echo "    make gui-test    Avalonia headless UI tests (podman)"
	@echo "    make lint        go vet across all modules (read-only)"
	@echo "    make reachability  D23: every bridge export consumed, every model surfaced"
	@echo "    make crossimpl-go  LIVE cross-impl: consume entity-core-go's signed root (podman)"
	@echo "    make fmt         gofmt -w over the tree (writes)"
	@echo "    make check       lint + test (the green gate)"
	@echo
	@echo "  Why test-each exists: 'make test' is fail-fast, so a count taken from a red"
	@echo "  run covers ONE package and is a lower bound (AP15). test-each runs all ten"
	@echo "  suites regardless and leaves per-suite logs in .test-logs/."
	@echo
	@echo "  -native variants run on a host Go toolchain; ARGS=… / *-box per the"
	@echo "  Makefile header. Platform: Linux is the only tested host (see README)."

# Pull the toolchain image (optional; `make build`/`test` auto-pull).
image:
	podman pull $(TOOLCHAIN_IMAGE)

# ============================================================
# doctor / run / gui / demo — the "can I actually use this" surface
# ============================================================
#
# These exist because everything below them assumed you already knew the
# answer. The build interface was complete and the ENTRY was not: there
# was no way to ask "is my machine set up", no single command that ran
# every suite to completion, and the GUI was reachable only as
# `make -C avalonia …` from a README line.

# doctor reports the environment instead of failing at module resolution
# twenty seconds into a build. Every check prints ok/WARN/FAIL and the
# target exits non-zero only on FAIL, so it is usable as a CI gate.
#
# The sibling check is the load-bearing one: every go.mod resolves the
# kernel through `replace ../../entity-core-go/{core,ext}`, so without
# that directory nothing in this repo builds, and the error you get is a
# module-resolution wall that does not mention the sibling.
.PHONY: doctor
doctor:
	@echo "entity-workbench-go — environment check"
	@echo
	@fail=0; \
	printf '  %-22s ' "podman"; \
	if command -v podman >/dev/null 2>&1; then echo "ok  $$(podman --version 2>/dev/null)"; \
	else echo "FAIL  not found — required for 'make build/test' and ALL Avalonia work"; fail=1; fi; \
	printf '  %-22s ' "go (host, optional)"; \
	if command -v go >/dev/null 2>&1; then echo "ok  $$(go version 2>/dev/null | cut -d' ' -f3) — '-native' targets available"; \
	else echo "warn  not found — fine; containerized targets supply go $(GOTOOLCHAIN)"; fi; \
	printf '  %-22s ' "sibling kernel"; \
	if [ -d "$(PARENT)/entity-core-go/core" ] && [ -d "$(PARENT)/entity-core-go/ext" ]; then \
		rev=$$(git -C "$(PARENT)/entity-core-go" rev-parse --short HEAD 2>/dev/null || echo "no-git"); \
		echo "ok  $(PARENT)/entity-core-go @ $$rev"; \
	else \
		echo "FAIL  $(PARENT)/entity-core-go missing (needs core/ and ext/)"; \
		echo "  $(shell printf '%22s' '')     every go.mod replaces the kernel to ../../entity-core-go;"; \
		echo "  $(shell printf '%22s' '')     without it the build dies at module resolution."; \
		fail=1; \
	fi; \
	printf '  %-22s ' "avalonia image"; \
	if podman image exists localhost/entity-avalonia:dev >/dev/null 2>&1; then echo "ok  localhost/entity-avalonia:dev built"; \
	else echo "warn  not built — run 'make gui-build' (first build is slow; pulls the .NET SDK)"; fi; \
	printf '  %-22s ' "binaries"; \
	if [ -x "$(CURDIR)/bin/entity-shell" ]; then echo "ok  ./bin/entity-shell present"; \
	else echo "warn  ./bin not populated — run 'make build'"; fi; \
	printf '  %-22s ' "host platform"; \
	case "$$(uname -s)" in \
		Linux) echo "ok  Linux — the only tested host";; \
		*) echo "warn  $$(uname -s) — untested; Linux is what CI and the GUI are exercised on";; \
	esac; \
	echo; \
	if [ $$fail -ne 0 ]; then echo "  -> blocking problems above. Fix those first."; exit 1; fi; \
	echo "  -> ready. Try 'make run' (CLI), 'make gui' (desktop), or 'make demo'."

# run / gui — the two entry points a person actually wants. `run` builds
# first so it is never a stale binary; ARGS forwards a one-shot command
# (`make run ARGS="name ls"`) instead of entering the REPL.
.PHONY: run gui gui-run gui-build gui-test
run: shell-build
	@$(BIN_DIR)/entity-shell $(ARGS)

# Avalonia lives behind podman and its own Makefile. These are thin
# passthroughs so the GUI is discoverable from `make help` at the root
# rather than from a README line — `up` is build + extract + launch.
#
# gui vs gui-run is the distinction worth knowing. `gui` rebuilds the
# image first — correct, and the only safe choice after a Go or C#
# change. `gui-run` launches the already-extracted binary and rebuilds
# NOTHING (extracting first if dist-native is missing). Before it
# existed, "start the thing I built five minutes ago" had no verb at
# the root and cost a full podman build.
gui:
	$(MAKE) -C avalonia up

gui-run:
	$(MAKE) -C avalonia host-run

gui-build:
	$(MAKE) -C avalonia build

gui-test:
	$(MAKE) -C avalonia test

# demo drives the shipped binary through a scripted tour in a throwaway
# HOME, so it validates the ACTUAL product end to end and leaves nothing
# behind. This is the thing to run when you want to see where we are.
#
# It names an -identity explicitly. That is no longer a workaround —
# sqlite without one now uses the "default" identity and persists
# correctly — but a scripted tour should show the peer it is operating
# as rather than rely on a default, and the demo is also the place a
# reader learns the flag exists.
.PHONY: demo
demo: shell-build
	@bash tools/demo.sh "$(BIN_DIR)/entity-shell"

build:
	$(call IN_CONTAINER,make build-native BIN_DIR=bin)

test:
	$(call IN_CONTAINER,make test-native)

# Tier-1 lint/fmt/check — same container-default / -native-opt-in split as
# build/test. lint is read-only (go vet); fmt writes (gofmt -w).
lint:
	$(call IN_CONTAINER,make lint-native)

fmt:
	$(call IN_CONTAINER,make fmt-native)

check: lint test

# reachability — D23's enforcement point. Three times now a
# renderer-neutral model has been complete, tested, and green with NO
# user-reachable path to it (the name arc's unregistered handler, the
# handler browser only tview drove, the PeerLiveness export no C# file
# referenced). Every layer's tests pass in that state, because the
# defect is the ABSENCE of an edge between two correct layers, and no
# test starting inside either one can see it.
#
# Two sweeps, both pure grep, both fast. Underscores are stripped from
# both sides in the second: the model is peer_liveness_model.go and its
# consumer is LivenessRender, so a literal snake_case match reports
# every model as orphaned — and a sweep that cries wolf gets ignored,
# which is this same failure one level up.
.PHONY: reachability
reachability:
	@echo "==> D23 reachability sweep"
	@miss=0; \
	for e in $$(grep -h '^//export ' avalonia/bridge/*.go | awk '{print $$2}' | sort -u); do \
	  grep -rq "\b$$e\b" avalonia/frontend/ avalonia/tests/ || { echo "  bridge export with no C# consumer: $$e"; miss=1; }; \
	done; \
	for f in workbench/*_model.go; do \
	  case "$$f" in *_test*) continue;; esac; \
	  m=$$(basename "$$f" _model.go); \
	  cat avalonia/frontend/Panels/*.cs console/*.go shellcmd/*.go 2>/dev/null \
	    | tr -d '_' | grep -qi "$$(echo $$m | tr -d '_')" \
	    || { echo "  workbench model no renderer or verb drives: $$m"; miss=1; }; \
	done; \
	if [ $$miss -ne 0 ]; then \
	  echo "  -> a model or export with no shipped surface is not shipped (D23)."; \
	  exit 1; \
	fi; \
	echo "  ok  every bridge export is consumed; every model has a surface"

# --- Single granular target, bare-box --------------------------------
#
# `make build` / `make test` self-containerize, but the granular targets
# (test-sdk, test-shell, test-shellcmd, shell-build, publish-build, …)
# call `go` directly: fine on a native-toolchain host, but
# `go: command not found` on a bare box (only make + podman). The `%-box`
# pattern rule runs ANY target inside the toolchain image, so a bare box
# runs the granular targets too:
#
#   make test-sdk-box                      # was: make test-sdk
#   make test-shellcmd-box                 # was: make test-shellcmd
#   make publish-build-box BIN_DIR=bin     # was: make publish-build
#   make test-shell-box ARGS="-run TestFoo"
#
# It mirrors the build/build-native split: the bare `test-sdk` stays the
# native worker (used directly on a Go host AND re-invoked inside the
# container by this rule); `test-sdk-box` is the containerized entry.
# NOT for the interactive/host targets (shell, publish-serve, console-run)
# — those need a tty / display / host LAN and stay host-side by design.
%-box:
	$(call IN_CONTAINER,make $* BIN_DIR=bin ARGS="$(ARGS)")

# --- Entity Shell (REPL + one-shot) ---
#
# `make shell` runs the entity-shell from source via `go run`, so you
# always get the latest code without managing a stale binary. Pass
# extra flags or one-shot args via ARGS, e.g.:
#
#   make shell                          # interactive REPL
#   make shell ARGS="ls"                # one-shot
#   make shell ARGS="--json info"       # JSON output, one-shot
#   make shell ARGS="-version"          # print build-stamped version + exit
#   make shell-build                    # produce a ./entity-shell binary with stamped version
#   make shell-test                     # run shellcmd + entitysdk tests
#
# See docs/architecture/USAGE-SHELL.md for usage examples.
#
# SHELL_VERSION is derived from git for stamp injection at build/run
# time via -ldflags. `--dirty` annotates if the worktree has uncommitted
# changes — useful for distinguishing "this is exactly v1.2.3" from
# "this is v1.2.3 plus local edits."
SHELL_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "(no-git)")
SHELL_LDFLAGS := -ldflags "-X main.version=$(SHELL_VERSION)"

shell:
	go run $(SHELL_LDFLAGS) ./shell/cmd/entity-shell $(ARGS)

shell-once: shell

shell-help:
	go run $(SHELL_LDFLAGS) ./shell/cmd/entity-shell help

shell-build: ensure-bindir
	go build $(SHELL_LDFLAGS) -o $(BIN_DIR)/entity-shell ./shell/cmd/entity-shell

shell-test:
	cd entitysdk && go test ./...
	cd shellcmd && go test ./...

# --- Workbench (shared library) ---

workbench-test:
	cd workbench && go test -v ./...

# --- Console (TUI, no CGo) ---

console-build: ensure-bindir
	cd console && go build -v -o $(BIN_DIR)/entity-console .

console-run: console-build
	$(BIN_DIR)/entity-console

# --- CDN corridor binaries (publish + vcs + fetch) ---
#
# Same posture as shell-build / console-build: each cd's into its own
# module so the per-module go.mod replace directives win cleanly, then
# emits the binary into BIN_DIR. Run `make build` to rebuild all of
# them; run any single one for a targeted rebuild.

publish-build: ensure-bindir
	cd entity-publish && go build -o $(BIN_DIR)/entity-publish .

# publish-serve — publish a peer's tree + serve the output dir over
# HTTP via python3 -m http.server so cohort validators on this LAN
# can fetch the http-poll origin and verify hash/manifest behaviour.
# Sits in the foreground until Ctrl-C.
#
# entity-publish prints the full §6.5.3 manifest, peer-id, and URL
# patterns on every run — share that block with the cohort so their
# validate-peer harnesses know what to target.
#
# Usage:
#   make publish-serve IDENTITY=alice
#   make publish-serve IDENTITY=alice PREFIX=docs/ PORT=8080
#   make publish-serve IDENTITY=alice IP=10.0.0.5
#
# Variables:
#   IDENTITY (required)  Named identity under ~/.entity/identities/.
#   PREFIX               Tree prefix to publish; default "" = whole tree.
#   PORT                 HTTP listen port. Default 8080.
#   IP                   Advertised LAN IP. Auto-detected from
#                        `hostname -I`; pass explicitly to override.
#   PUBLISH_OUT          Output directory. Default ./publish-out.
PORT ?= 8080
PUBLISH_OUT ?= ./publish-out
PREFIX ?=
IP ?= $(shell hostname -I 2>/dev/null | awk '{print $$1}')

publish-serve: publish-build
ifndef IDENTITY
	$(error IDENTITY is required, e.g. `make publish-serve IDENTITY=alice`)
endif
	@if [ -z "$(IP)" ]; then \
		echo "publish-serve: could not auto-detect LAN IP; pass IP=... explicitly"; \
		exit 1; \
	fi
	@rm -rf $(PUBLISH_OUT)
	$(BIN_DIR)/entity-publish \
		-identity $(IDENTITY) \
		-prefix '$(PREFIX)' \
		-out $(PUBLISH_OUT) \
		-origin http://$(IP):$(PORT)
	@echo ""
	@echo " serving $(PUBLISH_OUT) at http://$(IP):$(PORT) — Ctrl-C to stop"
	@echo ""
	@if ss -lnt 2>/dev/null | awk '{print $$4}' | grep -qE ':$(PORT)$$'; then \
		echo "publish-serve: port $(PORT) is already in use — clear it with:"; \
		echo "    pkill -f 'http.server $(PORT)'   # or"; \
		echo "    fuser -k $(PORT)/tcp"; \
		exit 1; \
	fi
	cd $(PUBLISH_OUT) && exec python3 -m http.server $(PORT)

vcs-build: ensure-bindir
	cd entity-vcs && go build -o $(BIN_DIR)/entity-vcs .

fetch-build: ensure-bindir
	cd entity-fetch && go build -o $(BIN_DIR)/entity-fetch .

ensure-bindir:
	@mkdir -p $(BIN_DIR)

# --- Canvas removed (Phase I Session 1) ---
# Raylib-based canvas renderer deleted; the workbench-go family is now
# console (TUI, frozen as discipline enforcer) + avalonia (primary GUI).
# See PHASE-I-MULTI-PEER-PLAN.md §3 and feedback-no-forced-renderer-parity.

# --- Top-level test + build (race-enabled, count=1 by default) ---
#
# See the docs at the top of this Makefile for usage examples.

# -timeout=30m: the default `go test` per-package timeout is 10m. The heavy
# multi-peer E2E convergence suite in shellcmd (TestE2E_Bidirectional_*,
# TestE2E_Burst*, …) is collectively slow under `-race` — individual cases run
# many seconds (e.g. TestE2E_Bidirectional_BurstThenTrigger ≈ 22.7s), and the
# documented ~17× modernc.org/sqlite-under-race penalty (see AGENTS.md "Perf
# measurement") compounds it, pushing the cumulative package runtime past the
# 10m default and panicking the suite (exit 2). Give generous headroom; this
# matches the perfreview target's -timeout=20m precedent.
GOTEST_FLAGS := -race -count=1 -timeout=30m

test-native: test-sdk test-shell test-shellboot test-shellcmd test-shellpanel test-workbench test-programs test-inspect test-publish test-fetch
	@echo "--- full sweep passed ---"

# ============================================================
# test-each — every suite to completion, then a summary
# ============================================================
#
# `test-native` is a prerequisite list, so make stops at the first suite
# that fails and every later suite is simply never run. That is correct
# fail-fast behavior and it is the wrong tool for "what is the state of
# the tree" — a failure count read through it covers ONE package and is
# a lower bound, which is AP15 in the discipline charter: we once
# reported two suites green that had 26 failures between them, and
# routed the numbers to another repo.
#
# The workaround has been a for-loop in AGENTS.md that every contributor
# had to know. This is that loop, as a target: it runs all ten
# regardless of failures, keeps per-suite logs, prints a table, and
# exits non-zero if any suite failed.
TEST_SUITES := sdk inspect shell shellboot shellcmd shellpanel workbench programs publish fetch
TEST_LOG_DIR := .test-logs

test-each:
	$(call IN_CONTAINER,make test-each-native)

test-each-native:
	@mkdir -p $(TEST_LOG_DIR)
	@echo "running $(words $(TEST_SUITES)) suites to completion (logs: $(TEST_LOG_DIR)/)"
	@echo
	@failed=""; \
	for t in $(TEST_SUITES); do \
		printf '  %-11s ' "$$t"; \
		start=$$(date +%s); \
		if $(MAKE) --no-print-directory test-$$t > $(TEST_LOG_DIR)/$$t.log 2>&1; then \
			printf 'PASS  %ss\n' "$$(( $$(date +%s) - start ))"; \
		else \
			printf 'FAIL  %ss\n' "$$(( $$(date +%s) - start ))"; \
			failed="$$failed $$t"; \
		fi; \
	done; \
	echo; \
	if [ -n "$$failed" ]; then \
		echo "FAILED:$$failed"; \
		for t in $$failed; do \
			echo; echo "  --- $$t: failing tests ---"; \
			grep -E '^\s*--- FAIL' $(TEST_LOG_DIR)/$$t.log | head -20 || true; \
			echo "  (full log: $(TEST_LOG_DIR)/$$t.log)"; \
		done; \
		exit 1; \
	fi; \
	echo "all $(words $(TEST_SUITES)) suites green"

# Native lint/fmt workers (used directly on a Go host AND re-invoked inside the
# toolchain image by the containerized lint/fmt targets above). lint = go vet
# across test-native's module set plus `publish`; fmt = gofmt -w over the whole
# tree (gofmt operates on files, so one pass covers every module). Note: the
# shipped-binary modules console/entity-{publish,vcs,fetch} are not vetted here
# — widen LINT_MODULES if lint should track the full `make build` ship set.
LINT_MODULES := entitysdk inspect shell shellboot shellcmd shellpanel workbench programs publish fetch

lint-native: lint-perfreview
	@for m in $(LINT_MODULES); do \
		echo "== vet $$m =="; (cd $$m && go vet $(ARGS) ./...) || exit 1; \
	done
	@echo "--- vet clean ---"

# perfreview rot guard. Every file in perfreview/ is `//go:build perfreview`, so
# it is invisible to `make test` and to the plain `go vet ./...` above — it once
# rotted silently against a kernel surface change and nothing caught it. This is
# compile-only ON PURPOSE: the benches take ~20m and must not run under -race
# (modernc.org/sqlite is ~17x slower there, so the numbers would be garbage —
# AGENTS.md). Vetting with the tag proves the module still compiles against the
# current kernel without paying for a run.
lint-perfreview:
	@echo "== vet perfreview (tagged) =="
	@cd perfreview && go vet -tags=perfreview ./... || exit 1

fmt-native:
	gofmt -w .
	@echo "--- gofmt -w done ---"

test-sdk:
	cd entitysdk && go test $(GOTEST_FLAGS) $(ARGS) ./...

test-inspect:
	cd inspect && go test $(GOTEST_FLAGS) $(ARGS) ./...

test-shell:
	cd shell && go test $(GOTEST_FLAGS) $(ARGS) ./...

test-shellboot:
	cd shellboot && go test $(GOTEST_FLAGS) $(ARGS) ./...

test-shellcmd:
	cd shellcmd && go test $(GOTEST_FLAGS) $(ARGS) ./...

test-shellpanel:
	cd shellpanel && go test $(GOTEST_FLAGS) $(ARGS) ./...

test-workbench:
	cd workbench && go test $(GOTEST_FLAGS) $(ARGS) ./...

# The entity-native programs track (generic host + descriptors + the
# Life/Snake/Asteroids/heavyfield programs). Extracted out of `workbench`
# so the app's renderer-neutral model layer stays the model layer.
test-programs:
	cd programs && go test $(GOTEST_FLAGS) $(ARGS) ./...

# The CDN corridor, both halves. `publish` was outside the sweep and
# `fetch` had no target at all until 2026-08-19 — which is how the two
# halves of one corridor drifted four ways apart while every suite in the
# sweep stayed green. A corridor with an untested end is an untested
# corridor.
test-publish:
	cd publish && go test $(GOTEST_FLAGS) $(ARGS) ./...

test-fetch:
	cd fetch && go test $(GOTEST_FLAGS) $(ARGS) ./...

# crossimpl-go — the LIVE cross-impl federation consume leg (C-7 / §1b).
#
# Deliberately NOT in `test-native`: it stands containers up on a podman
# bridge and needs the `entity-core-go` sibling checked out and buildable.
# A sweep target that needs a network and a neighbour's tree is a sweep
# target that goes red for reasons that are nobody's defect.
#
# What it does: their publisher (their script, unmodified) in its own
# container; OUR verifying consumer in a second container on the same
# bridge; manifest -> signature -> CHAMP walk -> leaves. See the script
# header for what a green run claims and — more importantly — what it
# does not.
crossimpl-go:
	bash scripts/crossimpl-go.sh

# perfreview — production-readiness measurement harness. Gated by the
# `perfreview` build tag (files use `//go:build perfreview`) so default
# `make test` skips them. No -race here: modernc.org/sqlite slows ~17×
# under the race detector per feedback_race_detector_vs_sqlite memo,
# which would distort every measurement.
perfreview:
	cd perfreview && go test -v -count=1 -tags=perfreview -timeout=20m $(ARGS) ./...

# `make build` builds every shipped binary (entity-shell, entity-console,
# CDN corridor tools). The Avalonia bridge .so is built separately by
# avalonia/bridge/build.sh; see avalonia/README.md.
#
# clean-strays runs first to nuke any dir-named binaries that `go
# build` drops when invoked directly inside a module dir without
# `-o`. The Makefile always uses `-o`, but past invocations + habit
# leave artifacts that get mistaken for current builds (cf. the
# `./console/console` incident where a May-9 binary was
# silently launched instead of today's `entity-console`).
build-native: clean-strays shell-build console-build publish-build vcs-build fetch-build
	@echo "--- built into $(BIN_DIR): entity-shell entity-console entity-publish entity-vcs entity-fetch ---"

# Nuke any binary named after its directory in a module dir. These
# are never produced by the Makefile — they only appear when someone
# (or a past version of this Makefile) ran `go build` without `-o`.
# Listing them out explicitly so it's obvious what's being removed.
clean-strays:
	@for p in console/console shell/shell workbench/workbench workbench/entity-console \
	         entity-publish/entity-publish entity-vcs/entity-vcs entity-fetch/entity-fetch \
	         entity-seed-site/entity-seed-site entity-serve-cors/entity-serve-cors \
	         canvas/canvas canvas/entity-canvas \
	         entity-shell; do \
		if [ -f "$$p" ]; then \
			echo "removing stray binary: $$p"; \
			rm -f "$$p"; \
		fi; \
	done

# Wipe all build outputs — canonical binaries + strays. Use after a
# rename or when you're unsure what's stale.
clean: clean-strays
	@rm -f entity-shell console/entity-console
	@rm -f $(BIN_DIR)/entity-shell $(BIN_DIR)/entity-console \
	       $(BIN_DIR)/entity-publish $(BIN_DIR)/entity-vcs $(BIN_DIR)/entity-fetch
	@echo "--- cleaned ($(BIN_DIR) + strays) ---"

# --- Generic go alias (escape hatch for any go subcommand) ---
#
# `make go ARGS="subcommand <flags> <pkgs>"` — see docs at the top of
# the Makefile for examples. Covers test/build/run/vet/mod/fmt/env/...
# without needing a separate target per subcommand.

go:
	go $(ARGS)
