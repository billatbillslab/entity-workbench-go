#!/usr/bin/env bash
# crossimpl-go.sh — workbench-go's leg of the multi-host cross-impl
# federation gate (entity-core-go C-7 / COHORT-OPEN-ITEMS §1b), the
# Go-reader half of the ask in
# entity-core-go/docs/status/HANDOFF-2026-08-20-workbench-go-federation-consume-leg.md.
#
# It stands up ENTITY-CORE-GO's publisher as its own container on a podman
# bridge (their script, unmodified — we never edit a sibling tree) and drives
# OUR verifying consumer at it from a second container on that same bridge.
#
#   manifest -> signature -> CHAMP trie walk from the signed root -> leaves
#
# WHAT A GREEN RUN CLAIMS: a separate network namespace, a distinct routable
# address, a real TCP hop, and hash + signature verification at a consumer that
# shares NO process and NO filesystem with the publisher. Two implementations on
# one chain.
#   IT DOES NOT CLAIM: two physical machines, the public internet, TLS, a CDN,
#   or NAT. Stated because a rig that overstates its scope is how a green gate
#   launders an untested claim.
#
# WHY THIS IS NOT browser-rust's RUN AGAIN. Theirs closed the same row from a
# Rust/browser consumer. This is the axis they structurally cannot cover: a
# reader in a DIFFERENT LANGUAGE than the emitter, over the same chain
# (ADR-0012 — a cohort all passing one author's vectors is cohort-consistent,
# not independent convergence).
#
# THREE INHERITED GOTCHAS, all handled below and each presenting as a broken
# server when it is not (browser-rust paid for these first; core-go's script
# carries two of them):
#   1. The host has no route into a rootless bridge — so the CONSUMER runs from
#      a container joined to the bridge, the vantage a real consumer has.
#   2. The bridge gateway is not the host — the publisher gets its own
#      container, which their script does.
#   3. The origin is baked in at publish time — their script reads the address
#      back before emitting the contract.
#
# FOURTH, OURS: their origin serves NO `transport-profile` object, so the layout
# cannot be discovered and is PINNED here (-pin-*). That is conformant of them
# (§6.5.4 makes profile distribution out-of-band in v1) and it is arch's R-28:
# a consumer handed only (origin, peer_id) has nowhere to learn `content_layout`
# from, and a WRONG GUESS IS BYTE-IDENTICAL TO A WITHHOLDING ORIGIN — every blob
# 404s either way. The pin is honest because it is explicit: these five values
# came from their handoff, not from a convention we invented.
#
#   bash scripts/crossimpl-go.sh          # up -> consume -> down
#   KEEP_UP=1 bash scripts/crossimpl-go.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
CORE_GO="${CORE_GO:-$REPO/../entity-core-go}"
NET="${FED_NET:-entity-go-fed}"
IMG="${FED_IMG:-docker.io/library/alpine:3.20}"
BIN="${CONSUMER_BIN:-/tmp/entity-workbench-consume}"

# Their origin's layout. Pinned, not discovered — see the note above.
PIN_CONTENT_LAYOUT="${PIN_CONTENT_LAYOUT:-flat}"
PIN_CONTENT_PREFIX="${PIN_CONTENT_PREFIX:-/content}"
PIN_MANIFEST_PREFIX="${PIN_MANIFEST_PREFIX:-/manifest}"
PIN_TREE_PREFIX="${PIN_TREE_PREFIX:-}"   # empty: origin-rooted, peer-id appended
PIN_LEAF_SUFFIX="${PIN_LEAF_SUFFIX:-.bin}"

# Reconciliation (trie-routed hash == the hash their own tree-leaf URL
# advertises) is ON by default: it is the clause only a consumer holding BOTH
# resolution paths can discharge, and it measured green against their origin on
# its first run (2026-08-20). NO_RECONCILE=1 drops it if a future origin stops
# exposing committed keys at leaf URLs — which would be a layout decision, not
# a defect.
RECONCILE_FLAG="-reconcile"
[ "${NO_RECONCILE:-0}" = "1" ] && RECONCILE_FLAG=""

log() { printf '\033[1m::\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31mFATAL:\033[0m %s\n' "$*" >&2; exit 1; }

[ -d "$CORE_GO" ] || die "sibling entity-core-go not found at $CORE_GO (set CORE_GO=)"
[ -x "$CORE_GO/scripts/federation-publish.sh" ] || \
  [ -f "$CORE_GO/scripts/federation-publish.sh" ] || \
  die "entity-core-go/scripts/federation-publish.sh missing — is that checkout current?"
command -v podman >/dev/null || die "podman is required"

cleanup() {
  if [ "${KEEP_UP:-0}" = "1" ]; then
    log "KEEP_UP=1 — leaving the publisher up; tear down with:"
    log "  bash $CORE_GO/scripts/federation-publish.sh down"
    return
  fi
  log "tearing the publisher down"
  bash "$CORE_GO/scripts/federation-publish.sh" down >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "building our verifying consumer (CGO off, linux/amd64) — it must run in a bare image"
( cd "$REPO/entity-fetch" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BIN" . )

log "standing up entity-core-go's federation publisher (their script, unmodified)"
CONTRACT="$(bash "$CORE_GO/scripts/federation-publish.sh" up)"
printf '%s\n' "$CONTRACT" >&2
# shellcheck disable=SC2046
eval "$CONTRACT"
[ -n "${FED_ORIGIN:-}" ] || die "no FED_ORIGIN in their contract"
[ -n "${FED_PEER_ID:-}" ] || die "no FED_PEER_ID in their contract"

log "their liveness probe first — a failure here is their rig, not our consumer"
bash "$CORE_GO/scripts/federation-publish.sh" probe

log "consuming $FED_ORIGIN from a container on $NET (peer $FED_PEER_ID)"
set +e
podman run --rm --network "$NET" -v "$BIN:/consume:z,ro" "$IMG" \
  /consume \
  -base "$FED_ORIGIN" \
  -peer-id "$FED_PEER_ID" \
  -verify -bodies $RECONCILE_FLAG \
  -absent "definitely/not/published/$$" \
  -pin-tree "$PIN_TREE_PREFIX" \
  -pin-content "$PIN_CONTENT_PREFIX" \
  -pin-manifest "$PIN_MANIFEST_PREFIX" \
  -pin-layout "$PIN_CONTENT_LAYOUT" \
  -pin-leaf "$PIN_LEAF_SUFFIX"
rc=$?
set -e

if [ $rc -ne 0 ]; then
  die "the consume run FAILED (exit $rc) — read the verdict above before blaming the rig;
       'tree/incomplete-walk' is a statement about the ORIGIN's closure, not about us"
fi

log "CROSS-IMPL CONSUME GREEN — a Go consumer walked entity-core-go's signed root"
log "across a real TCP hop, into a different network namespace, verifying the"
log "signature against the key their peer-id itself carries."
