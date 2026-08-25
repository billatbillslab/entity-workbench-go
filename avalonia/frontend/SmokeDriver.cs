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
        var snakeMode = Environment.GetEnvironmentVariable("WB_SMOKE_SNAKE");
        if (!string.IsNullOrEmpty(snakeMode))
        {
            return StartSnake(window, peer);
        }

        var lifeMode = Environment.GetEnvironmentVariable("WB_SMOKE_LIFE");
        if (!string.IsNullOrEmpty(lifeMode))
        {
            return StartLife(window, peer);
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

    // --- SNAKE mode (WB_SMOKE_SNAKE) ------------------------------------
    //
    // Drives the compute-program panel end-to-end under real X11:
    // switch the middle slot to the snake panel, start the host tick
    // clock, then steer a clockwise box via the input port so the game
    // survives the whole run (12x12 board, 6 ticks/s). Every surface a
    // user touches — panel mount, Start, input-port writes, wake→render
    // — runs for the full timer window; the run.log + frames carry the
    // proof. Honors WB_SMOKE_CYCLE_PATHS (steer count, default 50) and
    // WB_SMOKE_CYCLE_GAP_MS (steer gap, default 500ms ≈ one turn every
    // 3 ticks).
    private static bool StartSnake(MainWindow window, PeerView peer)
    {
        _window = window;
        _peer = peer;
        _cyclePaths = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_PATHS"), out var n) ? n : 50;
        _cycleGapMs = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_GAP_MS"), out var g) ? g : 500;
        Log($"snake mode: steers={_cyclePaths} gap={_cycleGapMs}ms");

        try
        {
            peer.SwitchMiddleSlotForSmoke("snake");
            Log("middle slot -> snake");
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
            StartSnakeCycle();
        };
        settle.Start();
        return true;
    }

    private static void StartSnakeCycle()
    {
        if (_peer == null) return;
        var snake = _peer.SnakeForSmoke;
        if (snake == null)
        {
            Log("middle slot is not snake; snake cycle aborted");
            return;
        }
        snake.StartForTests();
        Log("snake started (host tick clock running)");

        // Clockwise box: right, down, left, up — safe forever from the
        // seed (center of the board, heading right).
        var steers = new long[] { 2, 3, 0, 1 };
        _iteration = 0;
        _cycleTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(_cycleGapMs) };
        _cycleTimer.Tick += (_, _) =>
        {
            if (_peer == null)
            {
                _cycleTimer?.Stop();
                return;
            }
            var sp = _peer.SnakeForSmoke;
            if (sp == null)
            {
                _cycleTimer?.Stop();
                Log("snake panel disappeared mid-cycle; aborting");
                return;
            }
            if (_iteration >= _cyclePaths)
            {
                _cycleTimer?.Stop();
                Log($"snake cycle complete ({_iteration} steers) — final: {sp.StatusTextForTests}");
                return;
            }
            var dir = steers[_iteration % steers.Length];
            sp.InputForTests(dir);
            if ((_iteration % 8) == 0)
            {
                Log($"iter {_iteration}/{_cyclePaths} dir={dir} — {sp.StatusTextForTests}");
            }
            _iteration++;
        };
        _cycleTimer.Start();
    }

    // --- LIFE mode (WB_SMOKE_LIFE) --------------------------------------
    //
    // Drives the display-only compute-program panel end-to-end under
    // real X11: switch the middle slot to the life panel, start the host
    // tick clock, then sample the status line while it evolves (16x16
    // soup, 6 ticks/s). There is no input port to drive — Life is a
    // closed program — so the proof this run carries is the other half
    // of Snake's: panel mount, Start, wake→render, screenshots. Honors
    // WB_SMOKE_CYCLE_PATHS (status samples, default 20) and
    // WB_SMOKE_CYCLE_GAP_MS (sample gap, default 500ms ≈ 3 generations).
    private static bool StartLife(MainWindow window, PeerView peer)
    {
        _window = window;
        _peer = peer;
        _cyclePaths = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_PATHS"), out var n) ? n : 20;
        _cycleGapMs = int.TryParse(Environment.GetEnvironmentVariable("WB_SMOKE_CYCLE_GAP_MS"), out var g) ? g : 500;
        Log($"life mode: samples={_cyclePaths} gap={_cycleGapMs}ms");

        try
        {
            peer.SwitchMiddleSlotForSmoke("life");
            Log("middle slot -> life");
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
            StartLifeCycle();
        };
        settle.Start();
        return true;
    }

    private static void StartLifeCycle()
    {
        if (_peer == null) return;
        var life = _peer.LifeForSmoke;
        if (life == null)
        {
            Log("middle slot is not life; life cycle aborted");
            return;
        }
        life.StartForTests();
        Log("life started (host tick clock running)");

        _iteration = 0;
        _cycleTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(_cycleGapMs) };
        _cycleTimer.Tick += (_, _) =>
        {
            if (_peer == null)
            {
                _cycleTimer?.Stop();
                return;
            }
            var lp = _peer.LifeForSmoke;
            if (lp == null)
            {
                _cycleTimer?.Stop();
                Log("life panel disappeared mid-cycle; aborting");
                return;
            }
            if (_iteration >= _cyclePaths)
            {
                _cycleTimer?.Stop();
                Log($"life cycle complete ({_iteration} samples) — final: {lp.StatusTextForTests}");
                return;
            }
            // A soup can legitimately reach a fixed point mid-run; the
            // clock stopping is the model working, not a failure. Log it
            // and keep sampling so the frames still cover the window.
            Log($"iter {_iteration}/{_cyclePaths} — {lp.StatusTextForTests}");
            _iteration++;
        };
        _cycleTimer.Start();
    }

    private static void Log(string msg) => PanelLog.Write("smoke-driver", msg);
}
