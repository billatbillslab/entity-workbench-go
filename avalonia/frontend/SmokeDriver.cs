using System;
using System.IO;
using System.Runtime.InteropServices;
using System.Text.Json;
using Avalonia.Threading;
using EntityAvalonia.Panels;

namespace EntityAvalonia;

// SmokeDriver drives the UI programmatically under the Xvfb smoke
// harness (`make smoke-xvfb`). It exists to reproduce the rapid-
// click-on-large-doc scenario that caused the crash
// saga — except now against the adaptive render pipeline, under
// the real X11 + Skia paint path, with screenshots captured along
// the way.
//
// Activated by env vars:
//   WB_SMOKE_INGEST       — host path of a directory to ingest into
//                           the active peer (typically the
//                           docs/architecture dir mounted into the
//                           container). Triggers markdown-cycle mode.
//                           If unset AND no other mode is set, the
//                           smoke harness is just an idle boot test —
//                           which, with the default middle slot being
//                           site-view + EnsureDemoSite auto-seeding,
//                           still covers SITE idle paint.
//   WB_SMOKE_CYCLE_PATHS  — how many publish-cycle iterations to run
//                           across the ingested paths. Default 50.
//                           Set to 0 to disable cycling (ingest +
//                           tree-render only, no markdown stress).
//   WB_SMOKE_CYCLE_GAP_MS — ms between publishes. Default 150 so the
//                           150ms MarkdownView debounce sees real
//                           rapid changes.
//   WB_SMOKE_WINDOW       — resize | minimize | both. Layers ON TOP of
//                           any other mode: drives the window geometry
//                           while the primary mode paints. The only
//                           surface in this repo that can reach the
//                           open minimize crash (headless has no X11
//                           backend). See StartWindowCycle.
//   WB_SMOKE_CONNECTIONS  — drive the PEER-CONNECTIONS panel: mount it,
//                           then churn both renders (the local pool and
//                           the tree's liveness record) under real X11.
//                           Gates mount + handle allocation + render
//                           churn, NOT row content — one peer writes no
//                           lifecycle transitions. See StartConnections.
//   WB_SMOKE_SITE_NAVIGATE — set to non-empty to drive the SITE panel
//                           instead of markdown-view. Leaves the
//                           middle slot at site-view (its default) and
//                           cycles Navigate calls across the bundled
//                           demo's pages. Use to surface a Navigate-
//                           under-real-X11 regression that headless
//                           tests can't catch (paint stalls, GC
//                           pinning, dispatcher starvation).
//                           Honors WB_SMOKE_CYCLE_PATHS / GAP_MS.
//
// The driver runs entirely on the dispatcher; it does not spawn
// threads. Progress is logged to stderr via PanelLog so the smoke
// run.log carries a full breadcrumb of every cycle iteration.
public static class SmokeDriver
{
    private static MainWindow? _window;
    private static PeerView? _peer;
    private static string _ingestPath = "";
    private static int _cyclePaths;
    private static int _cycleGapMs;
    private static string[] _paths = Array.Empty<string>();
    private static int _iteration;
    private static DispatcherTimer? _cycleTimer;

    // Demo-site nav targets — match workbench/site_demo.go pages.
    // Each one resolves to a distinct location so the model fires
    // OnChange (it's idempotent on Navigate-to-current).
    private static readonly string[] _siteNavTargets = new[]
    {
        "./guide/intro",
        "./guide/install",
        "./guide/advanced/internals",
        "./about",
        "./theory",
        "./index",
    };

    // Called by MainWindow once the first PeerTab.View has been
    // constructed and added to the visual tree. Returns true if
    // smoke driving is active (env present), false otherwise. The
    // caller only needs to react to the env presence in case it
    // wants to log additional context.
    public static bool MaybeStart(MainWindow window, PeerView peer)
    {
        var primary = StartPrimaryMode(window, peer);

        // WB_SMOKE_WINDOW layers ON TOP of whatever primary mode ran,
        // rather than replacing it — the open minimize crash needs a
        // populated visual tree with paint in flight, so the useful run
        // is a program cycling WHILE the window collapses, not an idle
        // window collapsing alone.
        var windowMode = Environment.GetEnvironmentVariable("WB_SMOKE_WINDOW");
        if (!string.IsNullOrEmpty(windowMode))
        {
            StartWindowCycle(window, windowMode);
            return true;
        }
        return primary;
    }

    private static bool StartPrimaryMode(MainWindow window, PeerView peer)
    {
        // WB_SMOKE_SNAKE / _LIFE / _ASTEROIDS drove the three legacy
        // per-program panels and were retired with them on 2026-08-20.
        // WB_SMOKE_PROGRAM=snake|life|asteroids drives the same programs
        // through the generic host — one driver instead of three.

        // WB_SMOKE_PROGRAM=life|snake|asteroids — drives the GENERIC host panel.
        // One driver, every program. Same real X11 paint, same Skia, same bridge.
        var programMode = Environment.GetEnvironmentVariable("WB_SMOKE_PROGRAM");
        if (!string.IsNullOrEmpty(programMode))
        {
            return StartProgram(window, peer, programMode);
        }

        var handlersMode = Environment.GetEnvironmentVariable("WB_SMOKE_HANDLERS");
        if (!string.IsNullOrEmpty(handlersMode))
        {
            return StartHandlers(window, peer);
        }

        var connectionsMode = Environment.GetEnvironmentVariable("WB_SMOKE_CONNECTIONS");
        if (!string.IsNullOrEmpty(connectionsMode))
        {
            return StartConnections(window, peer);
        }

        var siteMode = Environment.GetEnvironmentVariable("WB_SMOKE_SITE_NAVIGATE");
        if (!string.IsNullOrEmpty(siteMode))
        {
            return StartSiteNavigate(window, peer);
        }

        var ingest = Environment.GetEnvironmentVariable("WB_SMOKE_INGEST");
        if (string.IsNullOrEmpty(ingest)) return false;
        if (!Directory.Exists(ingest))
        {
            Log($"WB_SMOKE_INGEST={ingest} does not exist; skipping driver");
            return false;
        }
        _window = window;
        _peer = peer;
        _ingestPath = ingest;
        _cyclePaths = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_PATHS"), out var n) ? n : 50;
        _cycleGapMs = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_GAP_MS"), out var g) ? g : 150;
        Log($"starting — ingest={ingest} cycles={_cyclePaths} gap={_cycleGapMs}ms");

        // Switch the middle slot to markdown-view so cycling actually
        // exercises the adaptive render pipeline (which is the whole
        // point of this run). Bottom slot stays at peer-info for
        // visible peer status.
        try
        {
            peer.SwitchMiddleSlotForSmoke("markdown-view");
            Log("middle slot -> markdown-view");
        }
        catch (Exception ex)
        {
            Log($"failed to switch middle slot: {ex.Message}");
            return false;
        }

        // Ingest. Use Bridge.DispatchLine on the active peer; the
        // path inside the tree is namespaced under `smoke/`.
        var cmd = $"ingest tree {ingest} smoke/";
        Log($"dispatch: {cmd}");
        var replyPtr = Bridge.DispatchLine(peer.PeerHandle, cmd);
        var reply = Marshal.PtrToStringAnsi(replyPtr) ?? "(null)";
        Bridge.FreeString(replyPtr);
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.GetProperty("ok").GetBoolean())
            {
                Log($"ingest failed: {reply}");
                return false;
            }
        }
        catch (Exception ex)
        {
            Log($"ingest reply parse failed: {ex.Message} reply={reply}");
            return false;
        }
        Log("ingest dispatched ok");

        // Filter the tree so the smoke prefix is visible. This also
        // makes the tree's _rows reflect the ingested entities.
        peer.TreeForSmoke.SetSearchForTests("smoke/");

        if (_cyclePaths <= 0)
        {
            Log("WB_SMOKE_CYCLE_PATHS=0 — ingest only, no cycle");
            return true;
        }

        // Wait briefly for the tree to populate, then start cycling.
        // 500ms is generous — typical ingest + tree-wake settling is
        // <100ms.
        var settle = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(500) };
        settle.Tick += (_, _) =>
        {
            settle.Stop();
            HarvestAndCycle();
        };
        settle.Start();
        return true;
    }

    private static void HarvestAndCycle()
    {
        if (_peer == null) return;
        var tree = _peer.TreeForSmoke;
        var paths = new System.Collections.Generic.List<string>();
        for (int i = 0; i < tree.RowsCountForTests; i++)
        {
            if (!tree.IsEntryForTests(i)) continue;
            var p = tree.GetRowPathForTests(i);
            if (!string.IsNullOrEmpty(p)) paths.Add(p);
        }
        _paths = paths.ToArray();
        Log($"harvested {_paths.Length} paths under smoke/");
        if (_paths.Length == 0)
        {
            Log("no paths ingested; cycle aborted");
            return;
        }

        _iteration = 0;
        _cycleTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(_cycleGapMs) };
        _cycleTimer.Tick += (_, _) => CycleStep();
        _cycleTimer.Start();
    }

    private static void CycleStep()
    {
        if (_peer == null || _paths.Length == 0)
        {
            _cycleTimer?.Stop();
            return;
        }
        if (_iteration >= _cyclePaths)
        {
            _cycleTimer?.Stop();
            Log($"cycle complete ({_iteration} publishes across {_paths.Length} paths)");
            return;
        }
        var path = _paths[_iteration % _paths.Length];
        if ((_iteration % 10) == 0)
        {
            Log($"iter {_iteration}/{_cyclePaths} -> {System.IO.Path.GetFileName(path)}");
        }
        _peer.PublishSelectedPath(path);
        _iteration++;
    }

    private static bool StartSiteNavigate(MainWindow window, PeerView peer)
    {
        _window = window;
        _peer = peer;
        _cyclePaths = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_PATHS"), out var n) ? n : 50;
        _cycleGapMs = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_GAP_MS"), out var g) ? g : 150;
        Log($"starting (SITE) — cycles={_cyclePaths} gap={_cycleGapMs}ms");

        // The default middle slot is already site-view (PeerView.cs).
        // EnsureDemoSite auto-seeds the bundled demo on the bridge's
        // SiteOpen call. So all we need to do is wait for the panel
        // to mount, then drive Navigate cycles across demo pages.
        if (_cyclePaths <= 0)
        {
            Log("WB_SMOKE_CYCLE_PATHS=0 — site idle only, no navigation cycle");
            return true;
        }

        var settle = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(500) };
        settle.Tick += (_, _) =>
        {
            settle.Stop();
            StartSiteCycle();
        };
        settle.Start();
        return true;
    }

    private static void StartSiteCycle()
    {
        if (_peer == null) return;
        var site = _peer.SiteForSmoke;
        if (site == null)
        {
            Log("middle slot is not site-view; site cycle aborted");
            return;
        }
        Log($"site cycle: {_siteNavTargets.Length} targets, {_cyclePaths} iterations");

        _iteration = 0;
        _cycleTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(_cycleGapMs) };
        _cycleTimer.Tick += (_, _) => SiteCycleStep();
        _cycleTimer.Start();
    }

    private static void SiteCycleStep()
    {
        if (_peer == null)
        {
            _cycleTimer?.Stop();
            return;
        }
        var site = _peer.SiteForSmoke;
        if (site == null)
        {
            _cycleTimer?.Stop();
            Log("site panel disappeared mid-cycle; aborting");
            return;
        }
        if (_iteration >= _cyclePaths)
        {
            _cycleTimer?.Stop();
            Log($"site cycle complete ({_iteration} navigations)");
            return;
        }
        var target = _siteNavTargets[_iteration % _siteNavTargets.Length];
        if ((_iteration % 10) == 0)
        {
            Log($"iter {_iteration}/{_cyclePaths} -> {target}");
        }
        site.NavigateForTests(target);
        _iteration++;
    }

    // --- HANDLER-BROWSER mode (WB_SMOKE_HANDLERS) -----------------------
    //
    // Drives HandlerBrowserPanel under real X11: mount it in the middle
    // slot, then walk the discovered handlers, selecting each and
    // executing its first operation. Every step repaints — the handler
    // list, the operation list, the spec line and a growing output log —
    // so the run exercises the thing headless cannot: the Skia paint
    // path under repeated ItemsSource churn.
    //
    // Executions will return error statuses. That is expected and is the
    // point: an arbitrary op dispatched with no params is exactly the
    // case whose error rendering nobody tests, and a panel that crashes
    // formatting a 400 is a panel that fails the first time a user is
    // exploring. Honors WB_SMOKE_CYCLE_PATHS (handlers to walk, default
    // 12) and WB_SMOKE_CYCLE_GAP_MS (default 250ms).
    private static bool StartHandlers(MainWindow window, PeerView peer)
    {
        _window = window;
        _peer = peer;
        _cyclePaths = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_PATHS"), out var n) ? n : 12;
        _cycleGapMs = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_GAP_MS"), out var g) ? g : 250;
        Log($"handlers mode: walk={_cyclePaths} gap={_cycleGapMs}ms");

        try
        {
            peer.SwitchMiddleSlotForSmoke("handler-browser");
            Log("middle slot -> handler-browser");
        }
        catch (Exception ex)
        {
            Log($"failed to switch middle slot: {ex.Message}");
            return false;
        }

        var settle = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(500) };
        settle.Tick += (_, _) =>
        {
            settle.Stop();
            StartHandlersCycle();
        };
        settle.Start();
        return true;
    }

    private static void StartHandlersCycle()
    {
        if (_peer == null) return;
        var hb = _peer.HandlersForSmoke;
        if (hb == null)
        {
            Log("middle slot is not handler-browser; handlers cycle aborted");
            return;
        }
        Log($"handler-browser mounted: {hb.HandlerCountForTests} handlers discovered");
        if (hb.HandlerCountForTests == 0)
        {
            Log("NO HANDLERS DISCOVERED — this run is NOT evidence the panel works");
        }

        _iteration = 0;
        _cycleTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(_cycleGapMs) };
        _cycleTimer.Tick += (_, _) =>
        {
            if (_peer == null)
            {
                _cycleTimer?.Stop();
                return;
            }
            var p = _peer.HandlersForSmoke;
            if (p == null)
            {
                _cycleTimer?.Stop();
                Log("handler-browser panel disappeared mid-cycle; aborting");
                return;
            }
            int limit = Math.Min(_cyclePaths, p.HandlerCountForTests);
            if (_iteration >= limit)
            {
                _cycleTimer?.Stop();
                Log($"handlers cycle complete ({_iteration} handlers walked, "
                    + $"{p.OutputCountForTests} output rows)");
                return;
            }
            p.SelectHandlerForTests(_iteration);
            var pattern = p.HandlerPatternAtForTests(_iteration);
            if (p.OperationCountForTests > 0)
            {
                p.ExecuteSelectedForTests();
                Log($"iter {_iteration}/{limit} — {pattern} "
                    + $"({p.OperationCountForTests} ops) → {p.OutputCountForTests} rows");
            }
            else
            {
                Log($"iter {_iteration}/{limit} — {pattern} (no operations)");
            }
            _iteration++;
        };
        _cycleTimer.Start();
    }

    // --- PEER-CONNECTIONS mode (WB_SMOKE_CONNECTIONS) --------------------
    //
    // Drives the panel that carries BOTH connection surfaces under real
    // X11: the local connection pool and the tree's liveness record
    // (`system/peer/status`). The liveness half is new as of 2026-08-20
    // and this is its tier-3 gate.
    //
    // **What this run can and cannot prove.** It proves the panel mounts,
    // both handles open, the render loop survives churn, and the
    // dispatcher is not starved — the class of fault AP24 caught in the
    // handler browser, which headless was structurally blind to. It does
    // NOT prove the liveness rows are correct: a lifecycle transition
    // needs a second peer, and a failed dial to a dead port never
    // establishes one, so this harness's status namespace stays empty by
    // construction. The log says so explicitly rather than letting an
    // empty list read as a pass.
    private static bool StartConnections(MainWindow window, PeerView peer)
    {
        _window = window;
        _peer = peer;
        _cyclePaths = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_PATHS"), out var n) ? n : 20;
        _cycleGapMs = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_GAP_MS"), out var g) ? g : 250;
        Log($"connections mode: cycles={_cyclePaths} gap={_cycleGapMs}ms");

        try
        {
            peer.SwitchMiddleSlotForSmoke("peer-connections");
            Log("middle slot -> peer-connections");
        }
        catch (Exception ex)
        {
            Log($"failed to switch middle slot: {ex.Message}");
            return false;
        }

        var settle = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(500) };
        settle.Tick += (_, _) =>
        {
            settle.Stop();
            StartConnectionsCycle();
        };
        settle.Start();
        return true;
    }

    private static void StartConnectionsCycle()
    {
        if (_peer == null) return;
        var cp = _peer.ConnectionsForSmoke;
        if (cp == null)
        {
            Log("middle slot is not peer-connections; connections cycle aborted");
            return;
        }
        Log($"peer-connections mounted: conns={cp.ConnectionCountForTests} "
            + $"liveness handle={cp.LivenessHandleForTests}");
        if (cp.LivenessHandleForTests < 0)
        {
            Log("LIVENESS HANDLE NOT ALLOCATED — this run is NOT evidence the section works");
        }

        _iteration = 0;
        _cycleTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(_cycleGapMs) };
        _cycleTimer.Tick += (_, _) =>
        {
            if (_peer == null)
            {
                _cycleTimer?.Stop();
                return;
            }
            var p = _peer.ConnectionsForSmoke;
            if (p == null)
            {
                _cycleTimer?.Stop();
                Log("peer-connections panel disappeared mid-cycle; aborting");
                return;
            }
            if (_iteration >= _cyclePaths)
            {
                _cycleTimer?.Stop();
                Log($"connections cycle complete ({_iteration} render churns) — "
                    + $"final: {p.LivenessHeaderForTests}");
                if (p.LivenessCountForTests == 0)
                {
                    Log("liveness list is EMPTY, which is correct here: this harness runs one "
                        + "peer, no transition was ever written, and absence is not disconnection "
                        + "(§5.4.1). Row CONTENT is not under test in this run.");
                }
                return;
            }
            // Both halves churn on every tick — the pool render clears an
            // ObservableCollection a ListBox is selecting into, which is
            // the exact shape that killed the handler browser under X11
            // and nowhere else (AP24).
            p.RerenderForTests();
            p.RerenderLivenessForTests();
            if ((_iteration % 5) == 0)
            {
                Log($"iter {_iteration}/{_cyclePaths} — conns={p.ConnectionCountForTests} "
                    + $"liveness={p.LivenessCountForTests} :: {p.LivenessHeaderForTests}");
            }
            _iteration++;
        };
        _cycleTimer.Start();
    }

    // --- GENERIC-HOST mode (WB_SMOKE_PROGRAM) ---------------------------
    //
    // Drives ProgramPanel for any program. The ONLY thing that varies is the
    // registry key; there is no per-program branch below, which is the same
    // claim the Go host makes, now asserted through real X11 + Skia.
    //
    // This is the strongest cheap evidence available for the rung: if the
    // vocabulary were secretly game-shaped, one of the three would need special
    // handling right here, and it does not. As of 2026-08-20 it is also the ONLY
    // program driver — the three per-program ones were retired with their panels.
    private static bool StartProgram(MainWindow window, PeerView peer, string program)
    {
        _window = window;
        _peer = peer;
        _cyclePaths = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_PATHS"), out var n) ? n : 20;
        _cycleGapMs = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_GAP_MS"), out var g) ? g : 500;
        Log($"program mode: program={program} samples={_cyclePaths} gap={_cycleGapMs}ms");

        var slot = program switch
        {
            "life" => "program-life",
            "snake" => "program-snake",
            "asteroids" => "program-asteroids",
            "life-edit" => "program-life-edit",
            _ => null,
        };
        if (slot == null)
        {
            Log($"WB_SMOKE_PROGRAM={program} is not one of life|snake|asteroids|life-edit");
            return false;
        }

        try
        {
            peer.SwitchMiddleSlotForSmoke(slot);
            Log($"middle slot -> {slot}");
        }
        catch (Exception ex)
        {
            Log($"failed to switch middle slot: {ex.Message}");
            return false;
        }

        var settle = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(500) };
        settle.Tick += (_, _) =>
        {
            settle.Stop();
            StartProgramCycle();
        };
        settle.Start();
        return true;
    }

    private static void StartProgramCycle()
    {
        if (_peer == null) return;
        var prog = _peer.ProgramForSmoke;
        if (prog == null)
        {
            Log("middle slot is not a ProgramPanel; program cycle aborted");
            return;
        }
        prog.StartForTests();
        Log($"program '{prog.ProgramNameForTests}' started (host tick clock running)");

        _iteration = 0;
        _cycleTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(_cycleGapMs) };
        _cycleTimer.Tick += (_, _) =>
        {
            if (_peer == null)
            {
                _cycleTimer?.Stop();
                return;
            }
            var pp = _peer.ProgramForSmoke;
            if (pp == null)
            {
                _cycleTimer?.Stop();
                Log("program panel disappeared mid-cycle; aborting");
                return;
            }
            if (_iteration >= _cyclePaths)
            {
                _cycleTimer?.Stop();
                Log($"program cycle complete ({_iteration} samples) — final: {pp.StatusTextForTests}");
                return;
            }
            // The status line carries the tick count and the bound shapes, so a
            // frozen tick or a missing shape shows up in the log without a
            // screenshot. A program reaching a fixed point is the program
            // working, not a failure.
            Log($"iter {_iteration}/{_cyclePaths} — {pp.StatusTextForTests}");
            _iteration++;
        };
        _cycleTimer.Start();
    }

    private static void Log(string msg) => PanelLog.Write("smoke-driver", msg);
    // --- WINDOW mode (WB_SMOKE_WINDOW) ------------------------------------
    //
    // Drives the window geometry itself: collapse toward zero and
    // restore, repeatedly, with whatever the primary mode is painting
    // still in flight.
    //
    // **This exists to reach a crash nothing else in this repo can
    // reach.** The open managed stack overflow (STATUS "Open bugs")
    // fires on window minimize on a real desktop, and its two
    // documented predecessors were a GridSplitter dragging a
    // star-weighted row to zero height — the same zero-size condition,
    // arriving from below instead of above. The headless suite tries
    // 25× collapse-to-0x0, 25× Minimized/restore and 40× collapse-with-
    // repaint-in-flight and survives all three, but **headless does not
    // run the X11 backend at all**, which is where both predecessors
    // lived. Xvfb does. This is the missing rung between the two.
    //
    //   WB_SMOKE_WINDOW=resize    — collapse ClientSize toward zero and
    //                               restore. The mode that genuinely
    //                               reproduces under Xvfb: a resize is
    //                               an X ConfigureWindow, which the
    //                               server honors with no WM present.
    //   WB_SMOKE_WINDOW=minimize  — cycle WindowState.Minimized/Normal.
    //                               **Read the log before believing a
    //                               pass**: iconify is a window-MANAGER
    //                               operation and the smoke harness runs
    //                               a bare Xvfb with no WM, so the state
    //                               may never take effect. Every step
    //                               logs the state and client size it
    //                               ACTUALLY observed afterwards, so a
    //                               run that proved nothing says so
    //                               instead of reading as a green gate.
    //   WB_SMOKE_WINDOW=both      — alternate the two.
    //
    // Honors WB_SMOKE_CYCLE_PATHS (iterations, default 40) and
    // WB_SMOKE_CYCLE_GAP_MS (default 250 — slower than the paint modes
    // because a geometry change has to round-trip through the X server
    // and back into a layout pass).
    private static DispatcherTimer? _windowTimer;
    private static string _windowMode = "resize";
    private static int _windowIteration;
    private static double _restoreWidth;
    private static double _restoreHeight;
    private static int _windowStateChanges;
    private static Avalonia.Size _lastObservedSize;
    private static Avalonia.Controls.WindowState _lastObservedState;
    private static bool _haveObservation;

    private static void StartWindowCycle(MainWindow window, string mode)
    {
        _window = window;
        _windowMode = mode.Trim().ToLowerInvariant() switch
        {
            "minimize" => "minimize",
            "both" => "both",
            _ => "resize",
        };
        _cyclePaths = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_PATHS"), out var n) ? n : 40;
        _cycleGapMs = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_GAP_MS"), out var g) ? g : 250;
        _restoreWidth = window.Width;
        _restoreHeight = window.Height;
        _windowIteration = 0;
        _windowStateChanges = 0;
        _haveObservation = false;
        Log($"WINDOW cycle starting — mode={_windowMode} iterations={_cyclePaths} gap={_cycleGapMs}ms " +
            $"restore={_restoreWidth}x{_restoreHeight}");

        // Let the primary mode settle and paint once before the
        // geometry starts moving; collapsing a window that has not laid
        // out yet tests a different thing than collapsing a live one.
        var settle = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(1200) };
        settle.Tick += (_, _) =>
        {
            settle.Stop();
            _windowTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(_cycleGapMs) };
            _windowTimer.Tick += (_, _) => WindowCycleStep();
            _windowTimer.Start();
        };
        settle.Start();
    }

    private static void WindowCycleStep()
    {
        var w = _window;
        if (w == null)
        {
            _windowTimer?.Stop();
            return;
        }
        if (_windowIteration >= _cyclePaths)
        {
            _windowTimer?.Stop();
            // Always leave the window restored — the harness screenshots
            // the final frame, and a 1x1 window would make every run
            // look like a paint failure.
            w.WindowState = Avalonia.Controls.WindowState.Normal;
            w.Width = _restoreWidth;
            w.Height = _restoreHeight;
            Log($"WINDOW cycle complete ({_windowIteration} iterations, " +
                $"{_windowStateChanges} observed state/size transitions)");
            if (_windowStateChanges == 0)
            {
                Log("WINDOW cycle observed ZERO transitions — the window never actually moved, so " +
                    "this run is NOT evidence about anything. Expected for mode=minimize under a " +
                    "bare Xvfb: iconify is a window-MANAGER operation and this harness runs no WM. " +
                    "mode=resize does move (an X ConfigureWindow needs no WM) — if THAT reports " +
                    "zero, the driver is broken, not the app.");
            }
            else
            {
                Log($"WINDOW cycle: {_windowStateChanges} transitions actually took effect, with " +
                    "paint in flight, and the app survived all of them.");
            }
            return;
        }

        var collapsing = (_windowIteration % 2) == 0;
        var useMinimize = _windowMode == "minimize" ||
                          (_windowMode == "both" && ((_windowIteration / 2) % 2) == 1);

        // Observe FIRST, and compare against the PREVIOUS tick's
        // observation — not against a same-tick readback.
        //
        // The same-tick version was wrong and the first run proved it:
        // an X11 geometry change is a request to the server that lands
        // asynchronously, so reading ClientSize immediately after
        // setting it returns the OLD value every time, and the counter
        // reported "zero transitions" for a run whose own log showed
        // 1400x900 and 1x1 alternating twenty times. A gate that
        // miscounts in the reassuring direction is worse than no gate:
        // it was one line away from reporting a run that never
        // collapsed anything as a run that survived collapsing.
        var before = w.ClientSize;
        var beforeState = w.WindowState;
        if (_haveObservation && (before != _lastObservedSize || beforeState != _lastObservedState))
        {
            _windowStateChanges++;
        }
        _lastObservedSize = before;
        _lastObservedState = beforeState;
        _haveObservation = true;

        try
        {
            if (useMinimize)
            {
                w.WindowState = collapsing
                    ? Avalonia.Controls.WindowState.Minimized
                    : Avalonia.Controls.WindowState.Normal;
            }
            else if (collapsing)
            {
                // Toward zero, not to zero: Avalonia clamps a 0 and the
                // predecessors both died on a viewport that had become
                // effectively-zero for the content, not literally 0.
                w.Width = 1;
                w.Height = 1;
            }
            else
            {
                w.Width = _restoreWidth;
                w.Height = _restoreHeight;
            }
        }
        catch (Exception ex)
        {
            Log($"WINDOW step {_windowIteration} threw: {ex.GetType().Name}: {ex.Message}");
        }

        // The log carries what was OBSERVED entering this tick and what
        // was REQUESTED in it. The pair is the honest record: a request
        // the server or the toolkit ignored shows up as an observation
        // that never moves, and a green run that moved nothing says so.
        if ((_windowIteration % 5) == 0 || _windowIteration < 4)
        {
            var want = useMinimize
                ? (collapsing ? "state->Minimized" : "state->Normal")
                : (collapsing ? "size->1x1" : $"size->{_restoreWidth:F0}x{_restoreHeight:F0}");
            Log($"WINDOW iter {_windowIteration}/{_cyclePaths} observed " +
                $"{beforeState}/{before.Width:F0}x{before.Height:F0}, requested {want}");
        }
        _windowIteration++;
    }

}
