#!/usr/bin/env bash
# demo.sh — a scripted tour of entity-shell against a throwaway peer.
#
# Run via `make demo`. The point is validation you can watch: every step
# below is the SHIPPED binary doing the thing, in a temporary HOME that
# is deleted on exit, so it answers "does this actually work on my
# machine" without touching anything you care about.
#
# Why -identity: without it, entity-shell generates a fresh keypair per
# invocation, so each command in this script would write into a
# different namespace of the same store and nothing would appear to
# persist. That is a real rough edge (tracked in docs/status/STATUS.md
# under "Hardening / cleanup"); the demo does the supported thing rather
# than hiding it.
#
# Every command is echoed before it runs. If a step fails the script
# stops and says which one — a demo that scrolls past its own errors is
# worse than no demo.

set -uo pipefail

SHELL_BIN="${1:?usage: demo.sh /path/to/entity-shell}"
if [ ! -x "$SHELL_BIN" ]; then
    echo "demo: $SHELL_BIN is not executable — run 'make build' first" >&2
    exit 1
fi

DEMO_HOME="$(mktemp -d -t entity-demo-XXXXXX)"
cleanup() { rm -rf "$DEMO_HOME"; }
trap cleanup EXIT

export HOME="$DEMO_HOME"
IDENTITY="demo"
STEP=0

# say prints a section header.
say() {
    printf '\n\033[1m== %s\033[0m\n' "$1"
}

# run echoes a shell command, runs it, and aborts the tour if it fails.
# The identity/storage flags are constant, so they are folded in here
# rather than repeated on every line of the script.
run() {
    STEP=$((STEP + 1))
    # %q per-argument, so an echoed command is one a reader can paste.
    # Plain "$*" drops the quoting and turns `-notes "a b c"` into three
    # bare words, which is a command that does something else.
    printf '\n\033[2m$ entity-shell'
    printf ' %q' "$@"
    printf '\033[0m\n'
    if ! "$SHELL_BIN" -identity "$IDENTITY" -storage sqlite "$@"; then
        printf '\n\033[31mdemo: step %d failed: %s\033[0m\n' "$STEP" "$*" >&2
        printf 'The tour stops here rather than scrolling past it.\n' >&2
        exit 1
    fi
}

printf '\033[1mentity-workbench-go — demo tour\033[0m\n'
printf 'Throwaway HOME: %s (deleted on exit)\n' "$DEMO_HOME"
printf 'Binary:         %s\n' "$SHELL_BIN"

say "1. Create an identity"
printf '\nA peer is a keypair. Without a named identity the shell makes an\n'
printf 'ephemeral one per invocation, so nothing persists between commands.\n'
printf '\n\033[2m$ entity-shell identity create %s\033[0m\n' "$IDENTITY"
if ! "$SHELL_BIN" identity create "$IDENTITY"; then
    echo "demo: identity create failed" >&2
    exit 1
fi

say "2. Who am I"
run peer ls

say "3. Put something in the tree and read it back"
run put demo/greeting test/scalar '"hello from the demo"'
run cat demo/greeting
run ls demo

say "4. Name resolution (EXTENSION-REGISTRY)"
printf '\nA local name book: names that resolve for THIS peer only. Nothing\n'
printf 'here is published or synced.\n'
run name ls
run name bind demo-self @demo -notes "bound by the demo tour"
run name ls
run name resolve demo-self

say "5. What the resolver will consult"
printf '\nThe dispatch list decides which backends a name SHAPE is eligible\n'
printf 'for. The catch-all is local-only by construction — that is the\n'
printf 'privacy MUST, not a default we picked.\n'
run name config

say "6. Names fail closed"
printf '\nAn unbound name reports the rung it stopped at, not a bare error.\n'
printf 'Expected to fail — that IS the demonstration.\n'
printf '\n\033[2m$ entity-shell name resolve nobody-bound-this\033[0m\n'
if "$SHELL_BIN" -identity "$IDENTITY" -storage sqlite name resolve nobody-bound-this 2>&1; then
    printf '\n\033[31mdemo: an unbound name RESOLVED — that is a bug\033[0m\n' >&2
    exit 1
fi

say "7. Clean up the binding"
run name unbind demo-self
run name unbind demo-self

printf '\n\033[1m== Tour complete\033[0m\n'
printf 'Every command above ran against the shipped binary.\n'
printf 'Note step 7: unbinding twice is reported honestly the second time.\n'
printf '\nNext:\n'
printf '  make gui            the desktop app (podman; first build is slow)\n'
printf '  make test-each      every Go suite to completion\n'
printf '  entity-shell help   the full verb list\n'
