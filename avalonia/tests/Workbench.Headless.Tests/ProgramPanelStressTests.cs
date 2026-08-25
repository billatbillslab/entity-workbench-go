using System;
using System.Threading.Tasks;
using Avalonia.Controls;
using Avalonia.Headless;
using Avalonia.Headless.XUnit;
using Avalonia.Threading;
using EntityAvalonia.Panels;
using Xunit;
using Xunit.Abstractions;

namespace EntityAvalonia.Tests;

// ProgramPanelStressTests — reproduce (or exonerate) the 64×64 sharded-Life
// SIGSEGV headlessly.
//
// The user hit a segfault ~15-20 s into running the "Life 64×64 (sharded host)"
// panel (make host-run). The core dump faults in managed code; the render of a
// 64×64 text grid was the prime suspect (see
// docs/status/HANDOFF-2026-07-18-avalonia-64x64-segfault.md). This suite drives
// that exact panel through REAL Skia rasterization (TestAppBuilder:
// UseHeadlessDrawing=false + UseSkia) with NO GPU/X11 — so the outcome is a
// diagnostic:
//
//   - crashes here            → the bug is our paint/Skia usage (managed);
//   - passes here, crashes GUI → the fault is the X11/GL platform layer (mesa
//                                driver), not our render code — work around it
//                                with software rendering, don't chase it here.
//
// It shows the panel in a window (forces the visual tree to rasterize), starts
// the program, and pumps far past the user's crash window while forcing repaints.
[Collection(nameof(BridgeCollection))]
public sealed class ProgramPanelStressTests
{
    private readonly BridgeFixture _bridge;
    private readonly ITestOutputHelper _output;

    public ProgramPanelStressTests(BridgeFixture bridge, ITestOutputHelper output)
    {
        _bridge = bridge;
        _output = output;
    }

    [AvaloniaFact]
    public async Task LifeBig_64x64_Renders_Under_Sustained_Repaint_Without_Crashing()
    {
        var panel = new ProgramPanel(_bridge.DefaultPeer, "life-big");
        Assert.Equal("life-big", panel.ProgramNameForTests);

        var window = new Window { Content = panel, Width = 520, Height = 600 };
        window.Show();
        HeadlessPump.Flush();

        // Start the host clock: ticks arrive from a Go goroutine, wake the panel,
        // and each wake invalidates the 64×64 grid → a real Skia repaint.
        panel.StartForTests();

        // Pump well past the ~15-20 renders the user's crash needed. Each cycle
        // ticks the render timer (rasterize) and drains the wake queue; the extra
        // InvalidateVisual hammers the paint path so we don't have to wait on the
        // slow ~1 Hz compute tick to accumulate repaints.
        const int cycles = 400;
        for (int i = 0; i < cycles; i++)
        {
            panel.InvalidateVisual();
            AvaloniaHeadlessPlatform.ForceRenderTimerTick();
            Dispatcher.UIThread.RunJobs();
            await Task.Delay(5);
        }

        _output.WriteLine($"life-big survived {cycles} repaint cycles; status: {panel.StatusTextForTests}");

        // If we got here the paint path did not segfault. Assert the panel is a
        // live surface (a crash would never reach this; a silently-broken mount
        // would show an error status).
        Assert.DoesNotContain("mount refused", panel.StatusTextForTests);
        Assert.DoesNotContain("author failed", panel.StatusTextForTests);

        ((IDisposable)panel).Dispose();
        window.Close();
    }
}
