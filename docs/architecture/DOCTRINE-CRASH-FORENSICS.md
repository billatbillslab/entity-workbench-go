# Doctrine — crash forensics on the Avalonia/.NET/cgo substrate

Canonical. Living doc — edit in place.

**Open this at the START of a crash investigation, not after the obvious
things fail.** Its entire value is that the intuitive order is wrong and
costs days: this repo spent a month on one crash, produced four honest
negative results, and every one of them was taken with an instrument that
could not reach the bug.

The charter (`DISCIPLINE-CHARTER.md`) says *what* must hold. The substrate
model (`MODEL-AVALONIA-RUNTIME.md`) says what the platform *does*. This
says *how to proceed* when the process dies and leaves nothing.

---

## §0 The cycle this names

> **Reach → capture → classify → bisect → mitigate → gate.**

The recurring failure it exists to prevent is inverting the first two.
Given a coredump, the reflex is to start reading it. But a dump you cannot
reproduce is a single sample of unknown representativeness, and on this
substrate it is very likely *mislabelled* (§3). **Reproduction is not the
last step of the investigation; it is the first**, because everything
downstream — bisection, mitigation, and the regression gate you finish
with — is gated on being able to run the thing again.

Each step below owns exactly one lever.

---

## §1 Reach — build the instrument before reading anything

**Lever: does any harness we own execute the region the bug lives in?**

Ask it as a *region* question, against the seven boundaries of
`MODEL-AVALONIA-RUNTIME.md §6`, and answer it in writing. The four
negatives that cost a month were all Boundary A/D (layout, window
geometry). The bug was Boundary G (signals), reachable only through real
input.

Concretely, this repo's instruments and what each **cannot** do:

| Instrument | Reaches | Cannot reach |
|---|---|---|
| `make -C avalonia test` (headless) | models, panel mount, envelope decode, real Skia raster | X11 backend at all; input dispatch; the window manager |
| `smoke-xvfb-{driver,site,handlers,connections,program}` | real X11 paint, real dispatcher, model-driven churn | **input dispatch, hit-testing, focus transfer** — every one of these calls the model method *under* the control |
| `smoke-xvfb-window` | window geometry, iconify with a WM | input; also compositing WMs (openbox is not mutter/kwin) |
| **`smoke-xvfb-click`** | **real pointer input via xdotool → hit-test → handlers** | keyboard-only paths; multi-touch; drag gestures (press+release at one point) |
| `make crash-hunt` | the above, swept over seeds, unattended | same limits, more samples |

**If no row reaches the region, building the missing instrument is the
task.** It is cheaper than it looks — `smoke-xvfb-click` was one package
(`xdotool`), ~40 lines of shell, and two make targets, and it hit on the
first seed after a month of misses.

**Every negative result is written with its reach in the same sentence**
(D24). "Survived 25 collapse cycles" is not a finding; "…and this harness
emits no X11 input, so it cannot reach input dispatch" is.

Determinism rules for the instrument itself:
- **Seed it, and log every event with its coordinates.** "It crashes
  randomly" is not actionable; "seed 7, click 143 at 812,455" is.
- **Do not trust a seed to reproduce.** This crash is timing-dependent:
  the same seed hit ~65% of runs. Sweep seeds; report the *rate*.
- **Write artifacts outside `dist-native/`** or copy them immediately —
  `extract` does `rm -rf` on it, so the next make invocation deletes the
  evidence.

---

## §2 Capture — the first signal, not the dump

**Lever: is the artifact you are about to read the actual fault?**

On this substrate, usually not. Run the reproducer under gdb and stop on
the **first** SIGSEGV with `nopass`:

```bash
make -C avalonia smoke-xvfb-click GDB=1 CLICK_SEED=7
```

`nopass` is the whole point: it keeps gdb from delivering the signal
onward, so the runtime never gets to handle, fail, and re-raise it. You
see the fault's own `rip`, `rsp` and `si_*`.

Two configuration traps that will waste a run:
- **Filter the runtime's own signals** or you stop on the wrong one:
  `handle all nostop noprint pass`, then re-enable SIGSEGV/SIGBUS. SIG34
  (`SIGRTMIN`) is CoreCLR injecting thread suspension and fires constantly.
- **gdb needs `--cap-add=SYS_PTRACE`** under podman.

Also capture, in the same run: `info proc mappings` (to identify which
mapping `rsp` is in — this is what distinguishes a managed-stack overflow
from a signal-stack one), and `info threads`.

---

## §3 Classify — read `si_code` before `si_addr`

**Lever: what kind of fault is this actually?**

**CoreCLR re-raises any fault it cannot classify.** The signal that lands
in a coredump is then the *second* one, carrying:

```
si_code = 128 (SI_KERNEL),  si_addr = 0,  registers = the handler's
```

Read naively that is a null dereference. It is not; it is an artefact.
This misreading is AP34 and it cost this repo the initial diagnosis.

| Observation | Means |
|---|---|
| `si_code 128` (SI_KERNEL), `si_addr 0` | **re-raised.** The dump tells you nothing about the fault site. Go to §2. |
| `si_code 1` (SEGV_MAPERR) | genuinely unmapped address — a real wild pointer |
| `si_code 2` (SEGV_ACCERR) | **permission**, not absence — guard page. Almost always a stack overflow |
| `si_addr == rsp - 8`, rip on a `call` | the pushed return address hit a guard page: **stack overflow, confirmed** |
| `rsp` inside a `PROT_NONE` mapping | same, and the mapping's neighbours tell you *which* stack |

Then ask **which stack**: if `rsp`'s region is a small (~16 KB) anonymous
mapping with a guard page, adjacent to libc/libpthread rather than to the
main `[stack]`, it is the **alternate signal stack**, and the overflow is
in signal handling. That is a different bug from a managed recursion, and
the runtime will not tell you: it prints no `Stack overflow.` and
`createdump` never fires, because there is no stack left to report on.

Three forensic channels that look productive and are dead ends here —
know them so you do not spend a day each:
- **The DAC will not load against a systemd ELF core** (`0x80004002`).
  There is no managed stack to be had from one. Only `createdump`'s own
  minidump carries what the DAC needs.
- **`dist-native/tools/dotnet-dump` cannot run on the host.** It is
  framework-dependent and sits beside a *self-contained* publish. Managed
  analysis runs inside the builder image (`make -C avalonia crash`).
- **`make crash` decodes Go symbols**, and these crashes have **zero**
  `libbridge.so` frames. A clean Go decode is not evidence of anything.

Cheap discriminators worth recording every time, so runs are comparable:

```bash
coredumpctl info $PID | grep -icE 'gallium|GLX_mesa|libGL\.|swrast'   # 21-ish = mesa driver bug; 0 = ours
coredumpctl info $PID | grep -c 'libbridge\.so'                        # 0 = not the Go bridge
```

---

## §4 Bisect — one variable, measured, never argued

**Lever: which hypotheses are actually excluded?**

With a reproducer in hand this is cheap, so **prefer an experiment over an
argument** (D19). Each run is one env var against the same binary and the
same seeds. Record the ruled-out list — it is as valuable as the answer,
and it is what stops the next session re-running them.

From the 2026-08-21 hunt, all ruled out as the trigger:

| Hypothesis | Test | Result |
|---|---|---|
| Go async preemption signals | `GODEBUG=asyncpreemptoff=1` | 5/5 still crashed |
| Background GC suspension churn | `DOTNET_gcConcurrent=0` | still crashed |
| Tiered-compilation rejit | `DOTNET_TieredCompilation=0` | still crashed |
| mesa/GPU driver | GPU-module discriminator | 0 modules, both dumps |
| the Go bridge | libbridge frame count | 0 frames, every dump |
| GridSplitter zero-size layout recursion (the documented predecessor) | grep the crashing runs' input breadcrumbs | **0** splitter presses in any of them |

**Measure the thing you are about to assert.** The altstack size was
*inferred* from a mapping table and then *measured* with `sigaltstack(2)`
at startup: 16384 bytes. The measurement is what made the fix defensible.

---

## §5 Mitigate — and be explicit about what is not explained

**Lever: does the change remove the crash, and do you know why?**

These are two questions and they can have different answers. Ship the
mitigation when the first is yes; **say so plainly when the second is no.**

The A/B is the deliverable, not the anecdote — same binary, same seeds,
one variable:

| arm | crashes |
|---|---|
| stock 16 KB altstack (`WB_ALTSTACK_BYTES=0`) | 6 / 8 seeds |
| 1 MB altstack (default) | 0 / 8 seeds |

**Keep the escape hatch that restores the broken behaviour.** Once a fix
lands, it is the only way anyone can re-measure the bug — and a mitigation
whose effect cannot be re-demonstrated becomes folklore in one session.

State the residue in the same breath as the fix: here, *what* consumes
more than 16 KB is still unidentified, and **only the UI thread is
protected** — every other managed thread still runs the stock size.

---

## §6 Gate — close the reach gap permanently

**Lever: would this bug survive a repeat?**

- The instrument built in §1 becomes a named target in `AGENTS.md` beside
  the suites, not a script in someone's shell history.
- The classification lesson becomes an anti-pattern with an enforcement
  point (AP34 → `GDB=1` mode; AP35 → `smoke-xvfb-click`).
- The substrate finding becomes a row in `MODEL-AVALONIA-RUNTIME.md §6`
  and an invariant in §7. **A new boundary is a real discovery** — six
  boundaries described every previously-diagnosed bug and none described
  this one, which was the tell that the map was short a row rather than
  that the bug was exotic.
- The app carries its own black box: breadcrumbs recorded unconditionally,
  input recorded *tunnelling* (before the target handler, or a crash in
  dispatch leaves no trace of the click), a durable crash log outside the
  wiped build directory, and the load-bearing runtime config announced on
  stderr at startup because it is the first thing a report needs.

---

## §7 The order, as a checklist

1. Name the region against `MODEL-AVALONIA-RUNTIME.md §6`. Does an
   instrument reach it? If not, **build one** — that is the task.
2. Reproduce. Seed it, log every event, sweep seeds, report the **rate**.
3. Catch the **first** signal under gdb with `nopass`.
4. `si_code` before `si_addr`. Identify **which stack** `rsp` is in.
5. Bisect by experiment, one variable, same seeds. Record the exclusions.
6. A/B the mitigation. Keep a switch that restores the bug.
7. Gate it, and state what remains unexplained.

**Anti-order** (what this doctrine exists to stop): read the dump →
theorise → run a harness that cannot reach the region → record a negative
→ conclude the bug is rare → repeat next month.
