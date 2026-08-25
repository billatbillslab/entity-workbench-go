# avalonia Makefile — podman-driven build, extract, and run.
#
# The build context is the parent of entity-workbench-go so the
# Containerfile can COPY both entity-core-go and entity-workbench-go
# siblings.

APP_DIR  := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
WB_DIR   := $(abspath $(APP_DIR)/..)
CTX_DIR  := $(abspath $(WB_DIR)/..)
IMAGE    := entity-avalonia:dev

# Podman resource caps — shared committed defaults live in the repo-root
# caps.mk (single source of truth, included by the root Makefile too). This
# is the heavy job implicated in the host crash, so every podman build/run
# below is fenced with $(PODMAN_BUILD_CAPS) / $(PODMAN_RUN_CAPS).
include $(WB_DIR)/caps.mk

.PHONY: help build run extract host-run up test smoke-xvfb smoke-xvfb-driver smoke-xvfb-site smoke-xvfb-snake smoke-xvfb-life crash clean

help:
	@echo "avalonia — podman build targets for the Avalonia desktop renderer"
	@echo ""
	@echo "  make up         build + extract + launch on host (one-shot — use this)"
	@echo "  make build      build the multi-stage image ($(IMAGE))"
	@echo "  make test       run headless UI tests (Avalonia.Headless.XUnit)"
	@echo "  make smoke-xvfb        run the real binary under Xvfb (real X11 paint, no display)"
	@echo "  make smoke-xvfb-driver same, but with auto-driver: ingest docs/architecture +"
	@echo "                         cycle through every doc in markdown-view"
	@echo "  make smoke-xvfb-hidpi  driver variant at 2560x1440 to catch HiDPI scaling bugs"
	@echo "  make smoke-xvfb-site   drive SITE panel: cycle Navigate across the demo site"
	@echo "  make smoke-xvfb-snake  drive SNAKE compute panel: tick clock + input-port steering"
	@echo "  make smoke-xvfb-life   drive LIFE compute panel: tick clock, display-only"
	@echo "  make crash      decode the most recent libbridge.so coredump"
	@echo "  make run        run the container with X11 forwarded from host"
	@echo "  make extract    copy /app out of the image to ./dist-native/"
	@echo "  make host-run   launch (extract if needed; does NOT rebuild)"
	@echo "  make clean      remove dist-native/ and the local image"
	@echo ""
	@echo "  context dir:  $(CTX_DIR)"
	@echo "  workbench:    $(WB_DIR)"
	@echo "  app:          $(APP_DIR)"

# Install our managed .containerignore at the build context root before
# every build. The canonical copy is committed inside avalonia/ so it
# travels with the repo; podman only reads .containerignore from the
# context root, hence the copy. Without this, every build invalidates
# the COPY layer (dist-native lives inside the workbench subtree).
build:
	@echo "==> building $(IMAGE) (context=$(CTX_DIR))"
	@cp $(APP_DIR)/containerignore $(CTX_DIR)/.containerignore
	podman build $(PODMAN_BUILD_CAPS) -t $(IMAGE) -f $(APP_DIR)/Containerfile $(CTX_DIR)

run:
	@echo "==> running $(IMAGE) with DISPLAY=$$DISPLAY"
	-xhost +local: 2>/dev/null || true
	podman run --rm -it $(PODMAN_RUN_CAPS) \
	    --userns=keep-id \
	    -e DISPLAY=$$DISPLAY \
	    -e XAUTHORITY=/tmp/.Xauth \
	    -v $$XAUTHORITY:/tmp/.Xauth:ro \
	    -v /tmp/.X11-unix:/tmp/.X11-unix \
	    --net=host \
	    --name entity-avalonia-run \
	    $(IMAGE)

extract:
	@rm -rf $(APP_DIR)/dist-native
	@mkdir -p $(APP_DIR)/dist-native
	@CID=$$(podman create $(IMAGE)) && \
	    podman cp $$CID:/app/. $(APP_DIR)/dist-native/ && \
	    podman rm $$CID > /dev/null
	@echo "==> extracted to $(APP_DIR)/dist-native/"
	@echo "    run: cd $(APP_DIR)/dist-native && LD_LIBRARY_PATH=. ./entity-avalonia"

# Headless UI tests. Builds up through the `tester` stage in the
# Containerfile; that stage runs `dotnet test` against
# Workbench.Headless.Tests. A test failure fails the podman build,
# which fails this target. No image is tagged.
test:
	@echo "==> running headless UI tests (target=tester)"
	@cp $(APP_DIR)/containerignore $(CTX_DIR)/.containerignore
	@# --no-cache to force the dotnet test step to actually re-run.
	@# Build artifacts inside earlier stages are still cached via
	@# layer hashes; only the final test invocation is forced.
	podman build $(PODMAN_BUILD_CAPS) --target tester --no-cache -f $(APP_DIR)/Containerfile $(CTX_DIR)

host-run:
	@if [ ! -x $(APP_DIR)/dist-native/entity-avalonia ]; then $(MAKE) extract; fi
	cd $(APP_DIR)/dist-native && LD_LIBRARY_PATH=. ./entity-avalonia

# The one command you actually want during iteration: rebuild the
# container image (podman caches unchanged stages, so this is fast
# if nothing moved), re-extract dist-native, launch.
#
# stderr+stdout are tee'd to dist-native/run.log so crash output
# is captured every time. On non-zero exit we surface the last 40
# lines of the log AND a `make crash` hint.
up: build extract
	@cp $(APP_DIR)/run-with-dump.sh $(APP_DIR)/dist-native/
	@chmod +x $(APP_DIR)/dist-native/run-with-dump.sh
	@echo "==> launching entity-avalonia; output -> $(APP_DIR)/dist-native/run.log"
	@cd $(APP_DIR)/dist-native && \
	  ./run-with-dump.sh 2>&1 | tee run.log; \
	  status=$${PIPESTATUS[0]}; \
	  if [ $$status -ne 0 ]; then \
	    echo ""; \
	    echo "==> entity-avalonia exited non-zero ($$status). Last 40 log lines:"; \
	    echo "----"; tail -40 run.log; echo "----"; \
	    echo "==> If it crashed, run \`make crash\` to decode."; \
	  fi

# Decode the most recent entity-avalonia coredump's libbridge.so
# backtrace to Go function names + file:line.  Symbols are
# kept in libbridge.so (no -s -w; see Containerfile).
crash:
	@echo "==> looking for the most recent entity-avalonia coredump..."
	@PID=$$(coredumpctl list -r --no-legend 2>/dev/null \
	    | grep entity-avalonia \
	    | head -1 \
	    | awk '{print $$5}'); \
	  if [ -z "$$PID" ]; then \
	    echo "no entity-avalonia coredump found in coredumpctl"; \
	    exit 1; \
	  fi; \
	  echo "    PID=$$PID"; echo ""; \
	  coredumpctl info $$PID 2>&1 \
	    | sed -n '/Stack trace/,/ELF object/p'; \
	  echo ""; \
	  echo "==> decoding libbridge.so addresses to Go symbols..."; \
	  coredumpctl info $$PID 2>&1 \
	    | grep -oE 'libbridge\.so \+ 0x[0-9a-f]+' \
	    | awk '{print $$3}' \
	    | while read addr; do \
	        echo -n "  $$addr -> "; \
	        addr2line -e $(APP_DIR)/dist-native/libbridge.so -f -C $$addr \
	          | tr '\n' '\t'; echo ""; \
	      done; \
	  echo ""; \
	  DMP=$$(ls -t $(APP_DIR)/dist-native/managed.*.dmp 2>/dev/null | head -1); \
	  if [ -z "$$DMP" ]; then \
	    echo "==> no managed minidump (managed.*.dmp) found."; \
	    echo "    Next time, \`make up\` sets DOTNET_DbgEnableMiniDump=1 — the"; \
	    echo "    crash will drop a dump that this target can analyze."; \
	  else \
	    echo "==> analyzing managed minidump: $$DMP"; \
	    echo "    (using bundled dotnet-dump from dist-native/tools/)"; \
	    cd $(APP_DIR)/dist-native && \
	      LD_LIBRARY_PATH=. \
	      ./tools/dotnet-dump analyze $$DMP \
	        -c 'clrstack -all' -c 'pe' -c 'exit' 2>&1 | head -200; \
	  fi

# Xvfb smoke run. Boots the REAL binary against the REAL X11 paint
# pipeline inside a virtual framebuffer — closes the gap between
# headless tests (in-process Skia, no X11 message loop) and what
# users actually run. Captures screenshots at ~2s intervals (frames/)
# + the full stderr/stdout log. Auto-closes via WB_SMOKE_EXIT_AFTER_SEC.
#
# Default timer 15s. Override with `make smoke-xvfb SECS=30`.
#
# Idle variant (this target) just boots + renders + idles. The
# `smoke-xvfb-driver` target adds programmatic driving on top.
SECS ?= 15
smoke-xvfb: build extract
	@cp $(APP_DIR)/run-xvfb-smoke.sh $(APP_DIR)/dist-native/
	@chmod +x $(APP_DIR)/dist-native/run-xvfb-smoke.sh
	@echo "==> Xvfb smoke run (timer=$(SECS)s)"
	@cd $(APP_DIR)/dist-native && \
	  podman run --rm $(PODMAN_RUN_CAPS) \
	    --userns=keep-id \
	    -v $(APP_DIR)/dist-native:/app:Z \
	    -w /app \
	    --entrypoint bash \
	    $(IMAGE) \
	    /app/run-xvfb-smoke.sh $(SECS); \
	  status=$$?; \
	  if [ $$status -ne 0 ]; then \
	    echo ""; \
	    echo "==> smoke-xvfb FAILED (exit $$status)"; \
	    echo "    artifacts in $(APP_DIR)/dist-native/xvfb-smoke-out/"; \
	    exit $$status; \
	  fi
	@echo "==> screenshot: $(APP_DIR)/dist-native/xvfb-smoke-out/screenshot.png"

# Driven variant — boots the app, then SmokeDriver inside the app
# ingests $(WB_DIR)/docs/architecture into the active peer, switches
# the middle panel slot to markdown-view, and cycles through every
# ingested doc N times. Reproduces the rapid-click-large-doc scenario
# the adaptive renderer was built for, under real X11 + Skia paint.
#
# Vars (override on command line):
#   SECS            run duration (sec)              default 30
#   CYCLES          publish-cycle iterations         default 60
#   GAP_MS          ms between publishes             default 150
DRIVER_SECS    ?= 30
DRIVER_CYCLES  ?= 60
DRIVER_GAP_MS  ?= 150
smoke-xvfb-driver: build extract
	@cp $(APP_DIR)/run-xvfb-smoke.sh $(APP_DIR)/dist-native/
	@chmod +x $(APP_DIR)/dist-native/run-xvfb-smoke.sh
	@echo "==> Xvfb smoke run with auto-driver (timer=$(DRIVER_SECS)s cycles=$(DRIVER_CYCLES))"
	@cd $(APP_DIR)/dist-native && \
	  podman run --rm $(PODMAN_RUN_CAPS) \
	    --userns=keep-id \
	    -v $(APP_DIR)/dist-native:/app:Z \
	    -v $(WB_DIR)/docs:/docs:Z,ro \
	    -e WB_SMOKE_INGEST=/docs/architecture \
	    -e WB_SMOKE_CYCLE_PATHS=$(DRIVER_CYCLES) \
	    -e WB_SMOKE_CYCLE_GAP_MS=$(DRIVER_GAP_MS) \
	    -w /app \
	    --entrypoint bash \
	    $(IMAGE) \
	    /app/run-xvfb-smoke.sh $(DRIVER_SECS); \
	  status=$$?; \
	  if [ $$status -ne 0 ]; then \
	    echo ""; \
	    echo "==> smoke-xvfb-driver FAILED (exit $$status)"; \
	    echo "    artifacts in $(APP_DIR)/dist-native/xvfb-smoke-out/"; \
	    exit $$status; \
	  fi
	@echo "==> frames: $(APP_DIR)/dist-native/xvfb-smoke-out/frames/"

# HiDPI smoke — same driver harness, but at 2560x1440x24. Catches
# layout/scaling issues that don't surface at the default 1280x1024
# (e.g., text-shape edge cases, panel proportions at higher DPI).
smoke-xvfb-hidpi: build extract
	@cp $(APP_DIR)/run-xvfb-smoke.sh $(APP_DIR)/dist-native/
	@chmod +x $(APP_DIR)/dist-native/run-xvfb-smoke.sh
	@echo "==> HiDPI Xvfb smoke run (2560x1440, timer=$(DRIVER_SECS)s)"
	@cd $(APP_DIR)/dist-native && \
	  podman run --rm $(PODMAN_RUN_CAPS) \
	    --userns=keep-id \
	    -v $(APP_DIR)/dist-native:/app:Z \
	    -v $(WB_DIR)/docs:/docs:Z,ro \
	    -e WB_SMOKE_INGEST=/docs/architecture \
	    -e WB_SMOKE_CYCLE_PATHS=$(DRIVER_CYCLES) \
	    -e WB_SMOKE_CYCLE_GAP_MS=$(DRIVER_GAP_MS) \
	    -e WB_SMOKE_SCREEN=2560x1440x24 \
	    -w /app \
	    --entrypoint bash \
	    $(IMAGE) \
	    /app/run-xvfb-smoke.sh $(DRIVER_SECS); \
	  status=$$?; \
	  if [ $$status -ne 0 ]; then \
	    echo ""; \
	    echo "==> smoke-xvfb-hidpi FAILED (exit $$status)"; \
	    echo "    artifacts in $(APP_DIR)/dist-native/xvfb-smoke-out/"; \
	    exit $$status; \
	  fi
	@echo "==> frames: $(APP_DIR)/dist-native/xvfb-smoke-out/frames/"

# SITE smoke — drives the SiteViewPanel under real X11 + Skia.
#
# The middle slot already defaults to site-view (PeerView.cs) and
# EnsureDemoSite auto-seeds the bundled demo on SiteOpen, so the
# fixture is automatic. The driver cycles Navigate across the demo's
# pages — surfaces Navigate-under-X11 regressions (paint stalls, GC
# pinning, dispatcher starvation) that headless tests miss.
#
# Tier-4 regression bar for the SITE thread (PHASE-I-SITE-VIEW-PLAN §7).
smoke-xvfb-site: build extract
	@cp $(APP_DIR)/run-xvfb-smoke.sh $(APP_DIR)/dist-native/
	@chmod +x $(APP_DIR)/dist-native/run-xvfb-smoke.sh
	@echo "==> SITE Xvfb smoke (timer=$(DRIVER_SECS)s cycles=$(DRIVER_CYCLES))"
	@cd $(APP_DIR)/dist-native && \
	  podman run --rm $(PODMAN_RUN_CAPS) \
	    --userns=keep-id \
	    -v $(APP_DIR)/dist-native:/app:Z \
	    -e WB_SMOKE_SITE_NAVIGATE=1 \
	    -e WB_SMOKE_CYCLE_PATHS=$(DRIVER_CYCLES) \
	    -e WB_SMOKE_CYCLE_GAP_MS=$(DRIVER_GAP_MS) \
	    -w /app \
	    --entrypoint bash \
	    $(IMAGE) \
	    /app/run-xvfb-smoke.sh $(DRIVER_SECS); \
	  status=$$?; \
	  if [ $$status -ne 0 ]; then \
	    echo ""; \
	    echo "==> smoke-xvfb-site FAILED (exit $$status)"; \
	    echo "    artifacts in $(APP_DIR)/dist-native/xvfb-smoke-out/"; \
	    exit $$status; \
	  fi
	@echo "==> frames: $(APP_DIR)/dist-native/xvfb-smoke-out/frames/"

# SNAKE smoke — drives the compute-program panel (SnakeGamePanel over
# wb.SnakeGameModel) under real X11: panel mount, host tick clock,
# input-port steering, wake→render, screenshots. The full
# compute-program loop (output port → display driver, input driver →
# input port) exercised headless.
smoke-xvfb-snake: build extract
	@cp $(APP_DIR)/run-xvfb-smoke.sh $(APP_DIR)/dist-native/
	@chmod +x $(APP_DIR)/dist-native/run-xvfb-smoke.sh
	@echo "==> SNAKE Xvfb smoke (timer=$(DRIVER_SECS)s steers=$(DRIVER_CYCLES))"
	@cd $(APP_DIR)/dist-native && \
	  podman run --rm $(PODMAN_RUN_CAPS) \
	    --userns=keep-id \
	    -v $(APP_DIR)/dist-native:/app:Z \
	    -e WB_SMOKE_SNAKE=1 \
	    -e WB_SMOKE_CYCLE_PATHS=$(DRIVER_CYCLES) \
	    -e WB_SMOKE_CYCLE_GAP_MS=$(DRIVER_GAP_MS) \
	    -w /app \
	    --entrypoint bash \
	    $(IMAGE) \
	    /app/run-xvfb-smoke.sh $(DRIVER_SECS); \
	  status=$$?; \
	  if [ $$status -ne 0 ]; then \
	    echo ""; \
	    echo "==> smoke-xvfb-snake FAILED (exit $$status)"; \
	    echo "    artifacts in $(APP_DIR)/dist-native/xvfb-smoke-out/"; \
	    exit $$status; \
	  fi
	@echo "==> frames: $(APP_DIR)/dist-native/xvfb-smoke-out/frames/"

# LIFE smoke — drives the display-only compute-program panel
# (LifeGamePanel over wb.LifeGameModel) under real X11: panel mount,
# host tick clock, wake→render, screenshots. The other half of the
# snake proof: no input port, so this exercises the output-port →
# display-driver path on its own.
smoke-xvfb-life: build extract
	@cp $(APP_DIR)/run-xvfb-smoke.sh $(APP_DIR)/dist-native/
	@chmod +x $(APP_DIR)/dist-native/run-xvfb-smoke.sh
	@echo "==> LIFE Xvfb smoke (timer=$(DRIVER_SECS)s samples=$(DRIVER_CYCLES))"
	@cd $(APP_DIR)/dist-native && \
	  podman run --rm $(PODMAN_RUN_CAPS) \
	    --userns=keep-id \
	    -v $(APP_DIR)/dist-native:/app:Z \
	    -e WB_SMOKE_LIFE=1 \
	    -e WB_SMOKE_CYCLE_PATHS=$(DRIVER_CYCLES) \
	    -e WB_SMOKE_CYCLE_GAP_MS=$(DRIVER_GAP_MS) \
	    -w /app \
	    --entrypoint bash \
	    $(IMAGE) \
	    /app/run-xvfb-smoke.sh $(DRIVER_SECS); \
	  status=$$?; \
	  if [ $$status -ne 0 ]; then \
	    echo ""; \
	    echo "==> smoke-xvfb-life FAILED (exit $$status)"; \
	    echo "    artifacts in $(APP_DIR)/dist-native/xvfb-smoke-out/"; \
	    exit $$status; \
	  fi
	@echo "==> frames: $(APP_DIR)/dist-native/xvfb-smoke-out/frames/"

clean:
	rm -rf $(APP_DIR)/dist-native
	-podman rmi $(IMAGE) 2>/dev/null || true
