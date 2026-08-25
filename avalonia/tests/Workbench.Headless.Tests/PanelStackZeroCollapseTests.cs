using System;
using System.Threading.Tasks;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Headless;
using Avalonia.Headless.XUnit;
using Avalonia.Threading;
using EntityAvalonia.Panels;
using Xunit;
using Xunit.Abstractions;

namespace EntityAvalonia.Tests;

// PanelStackZeroCollapseTests — the MINIMIZE crash, reduced to a headless
// layout pass (docs/status/HANDOFF-2026-07-18-avalonia-64x64-segfault.md,
// "UPDATE 2026-07-18 (later)").
//
// WHY THIS SUITE EXISTS. Three SIGSEGVs on this repo now share one signature:
// a MANAGED stack overflow — a tight alternating pair of JIT return addresses
// recursing until the .NET guard page — with no GPU/GL module anywhere in the
// faulting thread. Two were captured by an earlier session (PIDs 4033208,
// 4035311) when a GridSplitter drag drove a star-weighted row to ZERO size,
// and are documented in PanelStack's class doc as an upstream Avalonia
// layout-engine recursion. The third (PID 3513953) hit under
// WB_SOFTWARE_RENDER — which rules the mesa driver out — and the operator
// pinned the trigger: "mainly when I minimized the screen."
//
// Minimize drives the window to 0x0. That is the SAME zero-size condition, but
// it arrives FROM ABOVE (the window collapses the ScrollViewer viewport) rather
// than from a splitter drag below — so the RowDefinition.MinHeight/MaxHeight
// pins that made the drag path unreachable do not cover it.
//
// WHAT A RESULT MEANS. A .NET StackOverflowException is uncatchable and kills
// the process, so there is nothing to Assert.Throws on: the assertion IS
// surviving to the end of the test.
//
//   - crashes here  -> the recursion is reachable from a pure layout pass, is
//                      ours to clamp at the panel layer, and is now bisectable
//                      headlessly (no GPU, no X11, no window manager);
//   - passes here   -> a 0x0 layout pass alone is not sufficient; the trigger
//                      needs something the headless platform does not model
//                      (real WindowState transitions / X11 unmap).
//
// RESULT, 2026-07-22: ALL THREE PASS. The suite is therefore a NEGATIVE result,
// and it is kept as a regression fence, not as a reproducer. What it rules out:
// collapsing a shown, populated stack to 0x0 and back 25x, minimize/restore via
// WindowState 25x, and collapsing the 64x64 sharded-Life panel 40x WITH a live
// repaint stream in flight are all survivable in headless Skia. So whatever the
// GUI hits needs something this platform does not model — a real X11 unmap, a
// window-manager-driven WindowState transition, or a compositor interaction.
//
// Do NOT read this as "the layout hypothesis is dead." Headless Avalonia does
// not run the X11 backend at all, which is exactly where the two documented
// predecessors lived. It narrows the search, it does not close it.
//
// NEXT LEAD. The handoff's other lead — symbolizing the core dump — is CLOSED:
// the entity-avalonia dumps (PID 3513953 et al) have rotated out of
// /var/lib/systemd/coredump. Pinning this now needs a FRESH capture. When it
// next crashes, grab the dump before rotation and symbolize it in the .NET
// container (`dotnet-dump analyze` -> `clrstack`) to name the recursive pair.
//
// Tier 3 (headless integration) per docs/architecture/TESTING-STRATEGY.md.
[Collection(nameof(BridgeCollection))]
public sealed class PanelStackZeroCollapseTests
{
    private readonly BridgeFixture _bridge;
    private readonly ITestOutputHelper _output;

    public PanelStackZeroCollapseTests(BridgeFixture bridge, ITestOutputHelper output)
    {
        _bridge = bridge;
        _output = output;
    }

    // Collapse a shown, populated stack to 0x0 and back, repeatedly. This is
    // the minimize/restore cycle reduced to the only thing minimize does that
    // layout can see: the client size goes to zero and comes back.
    [AvaloniaFact]
    public void Stack_Survives_Window_Collapse_To_Zero_Size()
    {
        var host = new TestHost();
        var stack = new PanelStack(_bridge.DefaultPeer, host, "detail", "peer-info");
        var window = new Window { Content = stack, Width = 900, Height = 700 };
        try
        {
            window.Show();
            HeadlessPump.Flush();

            // Several cycles: the recursion the class doc describes needed a
            // layout pass to re-enter itself, so one collapse may settle where
            // a collapse/restore/collapse sequence keeps re-arming it.
            for (int i = 0; i < 25; i++)
            {
                window.Width = 0;
                window.Height = 0;
                HeadlessPump.Flush();
                stack.InvalidateMeasure();
                stack.InvalidateArrange();
                HeadlessPump.Flush();

                window.Width = 900;
                window.Height = 700;
                HeadlessPump.Flush();
            }

            _output.WriteLine($"survived 25 zero-collapse cycles; slots={stack.SlotCountForTests}");
            Assert.Equal(2, stack.SlotCountForTests);
        }
        finally
        {
            stack.Dispose();
            window.Close();
        }
    }

    // The same collapse driven through WindowState, which is what the operator
    // actually did. The headless platform may or may not model Minimized as a
    // real 0x0 client area — if it does not, this test is a cheap no-op and the
    // one above carries the diagnosis. Kept separate so a failure names which
    // path found it.
    [AvaloniaFact]
    public void Stack_Survives_WindowState_Minimize_Restore()
    {
        var host = new TestHost();
        var stack = new PanelStack(_bridge.DefaultPeer, host, "detail", "peer-info");
        var window = new Window { Content = stack, Width = 900, Height = 700 };
        try
        {
            window.Show();
            HeadlessPump.Flush();

            for (int i = 0; i < 25; i++)
            {
                window.WindowState = WindowState.Minimized;
                HeadlessPump.Flush();
                window.WindowState = WindowState.Normal;
                HeadlessPump.Flush();
            }

            _output.WriteLine($"survived 25 minimize/restore cycles; slots={stack.SlotCountForTests}");
            Assert.Equal(2, stack.SlotCountForTests);
        }
        finally
        {
            stack.Dispose();
            window.Close();
        }
    }

    // The 64x64 sharded-Life panel is the surface the operator was running when
    // it crashed, and it is the heaviest layout/paint client we have. Collapse
    // the window WHILE it is ticking: a live InvalidateVisual stream arriving at
    // a zero-size visual is the exact overlap the GUI session had and neither
    // test above reproduces.
    [AvaloniaFact]
    public async Task LifeBig_Survives_Collapse_While_Ticking()
    {
        var panel = new ProgramPanel(_bridge.DefaultPeer, "life-big");
        var window = new Window { Content = panel, Width = 520, Height = 600 };
        try
        {
            window.Show();
            HeadlessPump.Flush();
            panel.StartForTests();

            for (int i = 0; i < 40; i++)
            {
                // Collapse with paint in flight.
                window.Width = 0;
                window.Height = 0;
                panel.InvalidateVisual();
                AvaloniaHeadlessPlatform.ForceRenderTimerTick();
                Dispatcher.UIThread.RunJobs();

                window.Width = 520;
                window.Height = 600;
                panel.InvalidateVisual();
                AvaloniaHeadlessPlatform.ForceRenderTimerTick();
                Dispatcher.UIThread.RunJobs();
                await Task.Delay(5);
            }

            _output.WriteLine($"life-big survived 40 collapse-while-ticking cycles; status: {panel.StatusTextForTests}");
            Assert.DoesNotContain("mount refused", panel.StatusTextForTests);
        }
        finally
        {
            ((IDisposable)panel).Dispose();
            window.Close();
        }
    }

    // Minimal IPanelHost stub — mirrors PanelStackTests' TestHost (that one is
    // private to its own class, so this suite carries its own).
    private sealed class TestHost : IPanelHost
    {
        public event Action<string>? SelectedPath;
        public string? CurrentSelectedPath { get; private set; }
        public void PublishSelectedPath(string path)
        {
            CurrentSelectedPath = path;
            SelectedPath?.Invoke(path);
        }
        public void RequestPeerStatusRefresh() { }
    }
}
