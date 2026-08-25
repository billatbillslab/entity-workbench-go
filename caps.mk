# ============================================================================
# Podman resource caps — entity-systems standard
# (the numbers below are the standard; its rationale lives in an internal
#  coordination doc and is summarised here so this file stands alone).
#
# Per-container ceilings so a build/run can't take the host down. Added after
# a host hard-crash: concurrent podman builds exhausted memory + dragged the
# machine into swap-thrash. Zero-swap (CAP_SWAP == CAP_MEM) makes a runaway
# die cleanly at the cap instead of thrashing the desktop into a freeze.
#
# This file is the SINGLE committed source of the defaults. It is included by
# the root Makefile and avalonia/Makefile so both share one set of numbers.
#
#   Precedence (highest first):  env var  >  caps.local.mk  >  defaults below
#
# Per-machine override WITHOUT editing this file:
#   one-off:     CAP_MEM=4g CAP_CPUS=4 make -C avalonia build
#   persistent:  create  caps.local.mk  next to this file (gitignored), e.g.
#                  CAP_MEM  ?= 12g
#                  CAP_CPUS ?= 8
#                  CAP_CGROUP_PARENT ?= dev-heavy.slice
# ============================================================================

# Resolve caps.local.mk next to THIS file (repo root), regardless of which
# Makefile includes us or the current working directory.
CAPS_MK_DIR := $(dir $(lastword $(MAKEFILE_LIST)))
-include $(CAPS_MK_DIR)caps.local.mk

# --- Committed defaults (measured for THIS project) -------------------------
# CAP_MEM sized from the avalonia container build's measured peak memory
# pressure + ~25% headroom. The avalonia cold (--no-cache) build (fedora +
# golang + dotnet-sdk-8.0 + dotnet publish) measured a peak MemAvailable drop
# of ~3.05 GB (and that sample overlapped a concurrent test
# sweep, so the build alone is below that). 3.05 GB + 25% ≈ 3.8 GB -> 4g.
# The native `make build`/`make test` targets run no containers. Re-measure
# method recorded in RELEASE-READINESS.md "resource caps".
CAP_MEM           ?= 4g            # hard memory ceiling per container
CAP_SWAP          ?= $(CAP_MEM)    # keep == CAP_MEM (no swap); raise only deliberately
CAP_PIDS          ?= 4096          # max procs/threads (RUN only) — go+dotnet fork a lot
CAP_CPUS          ?= 6             # CPU cores at runtime (RUN only; fractional ok)
CAP_CGROUP_PARENT ?=              # optional host slice to nest under, e.g. dev-heavy.slice

_cap_cgp := $(if $(strip $(CAP_CGROUP_PARENT)),--cgroup-parent=$(CAP_CGROUP_PARENT),)

# podman BUILD accepts --memory/--memory-swap/--cgroup-parent (NOT --cpus/--pids-limit)
PODMAN_BUILD_CAPS := --memory=$(CAP_MEM) --memory-swap=$(CAP_SWAP) $(_cap_cgp)
# podman RUN accepts the full set
PODMAN_RUN_CAPS   := --memory=$(CAP_MEM) --memory-swap=$(CAP_SWAP) \
                     --pids-limit=$(CAP_PIDS) --cpus=$(CAP_CPUS) $(_cap_cgp)
