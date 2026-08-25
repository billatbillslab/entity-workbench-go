using System;
using System.Linq;
using System.Text;
using System.Threading.Tasks;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Headless;
using Avalonia.Headless.XUnit;
using Avalonia.Input;
using Avalonia.Interactivity;
using Avalonia.VisualTree;
using EntityAvalonia.Panels;
using Xunit;
using Xunit.Abstractions;

namespace EntityAvalonia.Tests;

// ProgramPanelInputTests — does a click on the on-screen controller actually
// reach the running program?
//
// **This is the first test in this repo that drives a panel through REAL input
// dispatch.** Every other driver here calls the model method *under* the control
// — NavigateForTests, HandlerBrowserModel, the window driver, and
// ProgramPanelStressTests' StartForTests. Those are good tests of models and, by
// construction, cannot execute hit-testing, pointer capture, or a handler that
// runs before the panel's own code. AP35/D24: that blind spot hid a fatal crash
// for a month, and it is the same blind spot that let "the buttons don't work"
// on interactive Life sit unexplained.
//
// So these press the button the way a person does: MouseDown/MouseUp at the
// d-pad's real coordinates, through Avalonia's own input pipeline, and then ask
// the PROGRAM whether it moved. Every layer is live — hit test, Button handlers,
// the held-key mask, the P/Invoke, the Go host's input queue, the compute step,
// the display projection, and the frame coming back.
//
// Two independent defects were found here on 2026-08-21, either of which alone
// makes the controller inert:
//
//   1. **The host sampled its input ports only at tick time**, so a press
//      shorter than the 166.7 ms tick interval was overwritten by its own
//      release before any tick read it. Measured on a clocked host: 1 of 6
//      clicks moved the cursor. Fixed in programs/host.go (the input queue),
//      pinned by programs/host_input_queue_test.go.
//   2. **Avalonia's Button marks PointerPressed/Released handled**, so the
//      panel's `btn.PointerPressed += ...` instance handlers never ran at all.
//      See HookPressRelease in ProgramPanel.cs.
//
// Defect 1 is invisible from C# and defect 2 is invisible from Go. Only a test
// that spans both ends of the wire sees either.
[Collection(nameof(BridgeCollection))]
public sealed class ProgramPanelInputTests
{
    private readonly BridgeFixture _bridge;
    private readonly ITestOutputHelper _output;

    // Interactive Life's board is 16x16 (programs/life.go) and its cursor is
    // drawn in its own display kinds — 2 over an empty cell, 3 over a live one
    // (programs/life_edit.go). The TEST is allowed to know this; the panel is not.
    private const int BoardWidth = 16;
    private const ulong CursorOverDead = 2;
    private const ulong CursorOverAlive = 3;

    public ProgramPanelInputTests(BridgeFixture bridge, ITestOutputHelper output)
    {
        _bridge = bridge;
        _output = output;
    }

    // Reports every stage of the input path, and puts the whole trace in the
    // failure message — so when this breaks again the answer is WHICH layer
    // dropped the press, not merely that one was dropped. Both sources into the
    // held-key mask are exercised: the on-screen controller and the keyboard.
    [AvaloniaFact]
    public async Task DPadPress_ReachesTheProgram_FromBothPointerAndKeyboard()
    {
        var panel = new ProgramPanel(_bridge.DefaultPeer, "life-edit");
        var window = new Window { Content = panel, Width = 760, Height = 860 };
        var log = new StringBuilder();
        try
        {
            window.Show();
            HeadlessPump.Flush();
            panel.StartForTests();

            var ready = await HeadlessPump.WaitUntil(
                () => CursorIndex(panel) >= 0 && FindAxisButton(panel, "right") != null,
                TimeSpan.FromSeconds(60));
            log.AppendLine($"ready={ready} status={panel.StatusTextForTests}");
            Assert.True(ready, log.ToString());
            HeadlessPump.Flush();

            var button = FindAxisButton(panel, "right")!;
            log.AppendLine($"button bounds={button.Bounds} enabled={button.IsEnabled} " +
                           $"hitTestVisible={button.IsHitTestVisible} visible={button.IsEffectivelyVisible}");

            var point = button.TranslatePoint(
                new Point(button.Bounds.Width / 2, button.Bounds.Height / 2), window)!.Value;
            var hit = window.InputHitTest(point);
            log.AppendLine($"click point={point} hitTest={hit?.GetType().Name ?? "(null)"} " +
                           $"hitIsButtonOrChild={(hit as Visual)?.GetSelfAndVisualAncestors().Contains(button)}");

            // Where does the routed event actually get to? A handler registered
            // with handledEventsToo sees the event even after Button marks it
            // handled, which is precisely the distinction that matters here.
            bool tunnel = false, bubble = false, bubbleHandledToo = false, released = false;
            button.AddHandler(InputElement.PointerPressedEvent, (_, _) => tunnel = true,
                RoutingStrategies.Tunnel);
            button.AddHandler(InputElement.PointerPressedEvent, (_, _) => bubble = true,
                RoutingStrategies.Bubble);
            button.AddHandler(InputElement.PointerPressedEvent, (_, _) => bubbleHandledToo = true,
                RoutingStrategies.Bubble, handledEventsToo: true);
            button.AddHandler(InputElement.PointerReleasedEvent, (_, _) => released = true,
                RoutingStrategies.Bubble, handledEventsToo: true);

            var before = CursorIndex(panel);
            window.MouseDown(point, MouseButton.Left);
            HeadlessPump.Flush();
            window.MouseUp(point, MouseButton.Left);
            HeadlessPump.Flush();

            log.AppendLine($"PointerPressed at the button: tunnel={tunnel} bubble={bubble} " +
                           $"bubbleHandledToo={bubbleHandledToo}; PointerReleased(handledToo)={released}");

            var movedByClick = await HeadlessPump.WaitUntil(
                () => CursorIndex(panel) != before, TimeSpan.FromSeconds(20));
            log.AppendLine($"CLICK: cursor {before} -> {CursorIndex(panel)} moved={movedByClick}");

            // The keyboard is the other source into the same seam (ActionKeys
            // maps the axis name "right" to Key.Right).
            panel.Focus();
            HeadlessPump.Flush();
            var beforeKey = CursorIndex(panel);
            window.KeyPressQwerty(PhysicalKey.ArrowRight, RawInputModifiers.None);
            HeadlessPump.Flush();
            var movedByKey = await HeadlessPump.WaitUntil(
                () => CursorIndex(panel) != beforeKey, TimeSpan.FromSeconds(20));
            log.AppendLine($"KEY: cursor {beforeKey} -> {CursorIndex(panel)} moved={movedByKey} " +
                           $"panelFocused={panel.IsFocused}");

            _output.WriteLine(log.ToString());
            Assert.True(movedByClick, "the on-screen d-pad did not reach the program.\n" + log);
            Assert.True(movedByKey, "the keyboard did not reach the program.\n" + log);
        }
        finally
        {
            ((IDisposable)panel).Dispose();
            window.Close();
        }
    }

    [AvaloniaFact]
    public async Task LifeEdit_DPadClick_MovesTheCursorExactlyOneCell()
    {
        var panel = new ProgramPanel(_bridge.DefaultPeer, "life-edit");
        var window = new Window { Content = panel, Width = 760, Height = 860 };
        try
        {
            window.Show();
            HeadlessPump.Flush();
            panel.StartForTests();

            // The controller is built from scene.keymap the first time a frame
            // carrying the input port arrives, so wait for the board AND the d-pad.
            var ready = await HeadlessPump.WaitUntil(
                () => CursorIndex(panel) >= 0 && FindAxisButton(panel, "right") != null,
                TimeSpan.FromSeconds(60));
            Assert.True(ready,
                $"no display frame + controller within 60s (status: {panel.StatusTextForTests})");
            HeadlessPump.Flush(); // let layout settle so Bounds are real

            var button = FindAxisButton(panel, "right")!;
            Assert.True(button.Bounds.Width > 0 && button.Bounds.Height > 0,
                "the d-pad 'right' button laid out to zero size — nothing could ever hit it");

            var before = CursorIndex(panel);
            var point = button.TranslatePoint(
                new Point(button.Bounds.Width / 2, button.Bounds.Height / 2), window);
            Assert.True(point.HasValue, "could not translate the d-pad button into window coordinates");

            // A real click: press and release with no waiting in between. That is
            // ~0 ms of hold, which is the case the host used to drop entirely.
            window.MouseDown(point!.Value, MouseButton.Left);
            HeadlessPump.Flush();
            window.MouseUp(point!.Value, MouseButton.Left);

            var moved = await HeadlessPump.WaitUntil(
                () => CursorIndex(panel) != before, TimeSpan.FromSeconds(30));
            var after = CursorIndex(panel);
            _output.WriteLine($"cursor {before} -> {after} (ticks {panel.TicksForTests}, {panel.StatusTextForTests})");
            Assert.True(moved,
                $"one real click on the d-pad left the cursor at {before}. Either the press never " +
                $"reached the program, or the tick never observed it.");

            // Right is +1 within the row; the board is toroidal, so it wraps in-row.
            var want = (before / BoardWidth) * BoardWidth + (before + 1) % BoardWidth;
            Assert.Equal(want, after);

            // ONE click is ONE cell. The step is edge-triggered off the previous
            // held-key mask, so a press observed for several ticks — or a release
            // that never got observed — shows up here as drift.
            await HeadlessPump.WaitUntil(() => false, TimeSpan.FromSeconds(2));
            Assert.Equal(want, CursorIndex(panel));
        }
        finally
        {
            // A failing assert must not leave the host clock ticking: a leaked
            // running program keeps writing entities and invalidating visuals for
            // the rest of the assembly, which is how one real failure becomes six.
            ((IDisposable)panel).Dispose();
            window.Close();
        }
    }

    [AvaloniaFact]
    public async Task LifeEdit_RepeatedDPadClicks_EachMoveTheCursorExactlyOneCell()
    {
        var panel = new ProgramPanel(_bridge.DefaultPeer, "life-edit");
        var window = new Window { Content = panel, Width = 760, Height = 860 };
        try
        {
            window.Show();
            HeadlessPump.Flush();
            panel.StartForTests();

            var ready = await HeadlessPump.WaitUntil(
                () => CursorIndex(panel) >= 0 && FindAxisButton(panel, "right") != null,
                TimeSpan.FromSeconds(60));
            Assert.True(ready, $"no display frame + controller within 60s (status: {panel.StatusTextForTests})");
            HeadlessPump.Flush();

            var button = FindAxisButton(panel, "right")!;
            var point = button.TranslatePoint(
                new Point(button.Bounds.Width / 2, button.Bounds.Height / 2), window)!.Value;

            // Six clicks, each waited out — the sequence the operator actually
            // performs. Through the pre-fix host this landed 1 of 6.
            var start = CursorIndex(panel);
            for (int i = 0; i < 6; i++)
            {
                var was = CursorIndex(panel);
                window.MouseDown(point, MouseButton.Left);
                HeadlessPump.Flush();
                window.MouseUp(point, MouseButton.Left);

                var stepped = await HeadlessPump.WaitUntil(
                    () => CursorIndex(panel) != was, TimeSpan.FromSeconds(30));
                Assert.True(stepped, $"click {i + 1} of 6 did not move the cursor from {was}");

                // Let the release drain too, so the next click is a fresh 0->1 edge.
                await HeadlessPump.WaitUntil(() => false, TimeSpan.FromMilliseconds(700));
            }

            var after = CursorIndex(panel);
            var want = (start / BoardWidth) * BoardWidth + (start + 6) % BoardWidth;
            _output.WriteLine($"six clicks: cursor {start} -> {after} (want {want})");
            Assert.Equal(want, after);
        }
        finally
        {
            ((IDisposable)panel).Dispose();
            window.Close();
        }
    }

    // CursorIndex reads the cursor's cell index out of the last rendered frame,
    // which is the only place a test can see it — the cursor is program state,
    // and the panel is not allowed to know it exists.
    private static int CursorIndex(ProgramPanel panel)
    {
        var kinds = panel.DisplayKindsForTests("display");
        if (kinds == null) return -1;
        for (int i = 0; i < kinds.Length; i++)
        {
            if (kinds[i] == CursorOverDead || kinds[i] == CursorOverAlive) return i;
        }
        return -1;
    }

    // The d-pad buttons carry their axis name as Content (ProgramPanel
    // BuildController), which is what makes them findable without the test
    // reaching into panel internals.
    private static Button? FindAxisButton(ProgramPanel panel, string axis) =>
        panel.GetVisualDescendants()
             .OfType<Button>()
             .FirstOrDefault(b => b.Content as string == axis);
}
