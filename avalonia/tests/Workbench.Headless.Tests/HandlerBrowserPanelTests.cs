using System;
using System.Threading.Tasks;
using Avalonia.Controls;
using Avalonia.Headless;
using Avalonia.Headless.XUnit;
using Avalonia.Threading;
using EntityAvalonia.Panels;
using Xunit;

namespace EntityAvalonia.Tests;

// Headless tests for HandlerBrowserPanel — the panel that closes the
// last console→Avalonia parity gap.
//
// What these can and cannot reach: headless runs the full managed panel
// against a real bridge and a real peer, so mount, discovery, selection
// and dispatch are all genuinely exercised. What it does not run is the
// X11 backend, so these are not evidence about the render path — the
// same boundary every other headless suite here sits behind, stated so
// a green run is not read as more than it is.
[Collection(nameof(BridgeCollection))]
public sealed class HandlerBrowserPanelTests
{
    private readonly BridgeFixture _bridge;

    public HandlerBrowserPanelTests(BridgeFixture bridge)
    {
        _bridge = bridge;
    }

    private static async Task<HandlerBrowserPanel> MountAsync(long peer)
    {
        var panel = new HandlerBrowserPanel(peer, null);
        var window = new Window { Content = panel, Width = 700, Height = 500 };
        window.Show();

        var deadline = DateTime.UtcNow.AddSeconds(3);
        while (panel.HandlerCountForTests == 0 && DateTime.UtcNow < deadline)
        {
            AvaloniaHeadlessPlatform.ForceRenderTimerTick();
            Dispatcher.UIThread.RunJobs();
            await Task.Delay(10);
        }
        return panel;
    }

    // Mount must discover the peer's handlers without any user action.
    // Every AppPeer registers a substantial system handler set, so an
    // empty list here means discovery or the JSON projection is broken —
    // not that the peer is bare.
    [AvaloniaFact]
    public async Task Mount_Discovers_Handlers()
    {
        var panel = await MountAsync(_bridge.DefaultPeer);

        Assert.True(panel.HandleForTests > 0,
            $"handlers handle not allocated (got {panel.HandleForTests})");
        Assert.True(panel.HandlerCountForTests > 0,
            "no handlers discovered; every peer registers a system handler set");
        Assert.False(string.IsNullOrEmpty(panel.HandlerPatternAtForTests(0)),
            "first discovered handler has an empty pattern");
    }

    // Selecting a handler must populate its operation list. This is the
    // seam the JSON projection exists for: the model carries Specs as a
    // map behind a pointer, and a projection that dropped it would leave
    // the panel showing handlers you cannot do anything with.
    [AvaloniaFact]
    public async Task Selecting_A_Handler_Populates_Its_Operations()
    {
        var panel = await MountAsync(_bridge.DefaultPeer);
        Assert.True(panel.HandlerCountForTests > 0);

        // Find a handler that actually declares operations — some may
        // legitimately declare none, and asserting on index 0 blindly
        // would make this test depend on registration order.
        int found = -1;
        for (int i = 0; i < panel.HandlerCountForTests; i++)
        {
            panel.SelectHandlerForTests(i);
            Dispatcher.UIThread.RunJobs();
            if (panel.OperationCountForTests > 0) { found = i; break; }
        }

        Assert.True(found >= 0,
            "no discovered handler exposed any operation through the bridge projection");
        Assert.False(string.IsNullOrEmpty(panel.OperationNameAtForTests(0)),
            "operation row has an empty name");
        // With a handler and an operation selected the model produces a
        // spec line, and Execute becomes available. Before that it must
        // not be — a disabled button is the affordance that says "pick
        // an operation first".
        Assert.True(panel.ExecuteEnabledForTests,
            "Execute stayed disabled with an operation selected");
    }

    // Execution must append to the output log and must not throw. The
    // dispatch may well return an error status — that is a legitimate
    // outcome for an arbitrary op with no params, and the panel's job is
    // to render it, not to avoid it.
    [AvaloniaFact]
    public async Task Executing_Selected_Operation_Appends_Output()
    {
        var panel = await MountAsync(_bridge.DefaultPeer);

        for (int i = 0; i < panel.HandlerCountForTests; i++)
        {
            panel.SelectHandlerForTests(i);
            Dispatcher.UIThread.RunJobs();
            if (panel.OperationCountForTests > 0) break;
        }
        Assert.True(panel.OperationCountForTests > 0, "no operation to execute");

        int before = panel.OutputCountForTests;
        panel.ExecuteSelectedForTests();
        Dispatcher.UIThread.RunJobs();

        Assert.True(panel.OutputCountForTests > before,
            $"execute produced no output rows (before={before}, after={panel.OutputCountForTests})");
    }

    // The custom-dispatch escape hatch must refuse an empty URI by
    // logging rather than dispatching, and must survive a nonsense
    // target. Both are the model's rules; this checks they survive the
    // bridge round trip instead of surfacing as a crash.
    [AvaloniaFact]
    public async Task Custom_Dispatch_Refuses_Empty_And_Survives_Nonsense()
    {
        var panel = await MountAsync(_bridge.DefaultPeer);

        int before = panel.OutputCountForTests;
        panel.ExecuteCustomForTests("", "", "");
        Dispatcher.UIThread.RunJobs();
        Assert.True(panel.OutputCountForTests > before,
            "empty custom dispatch logged nothing; the refusal must be visible");

        before = panel.OutputCountForTests;
        panel.ExecuteCustomForTests("system/definitely-not-a-handler", "nope", "");
        Dispatcher.UIThread.RunJobs();
        Assert.True(panel.OutputCountForTests > before,
            "dispatch to an unknown handler produced no output");
    }

    // Walk EVERY handler, selecting and executing each — the exact loop
    // the X11 smoke driver runs.
    //
    // This test exists because the first version of this suite stopped at
    // the first handler that had operations, which was index 0, so it
    // never changed the selection at all. The driver did, and stopped
    // dead after one iteration. A test that asserts on the first item is
    // blind to every bug that needs a second one.
    [AvaloniaFact]
    public async Task Walking_Every_Handler_Survives_Selection_Churn()
    {
        var panel = await MountAsync(_bridge.DefaultPeer);
        int count = panel.HandlerCountForTests;
        Assert.True(count > 1, $"need >1 handler to exercise selection change; got {count}");

        for (int i = 0; i < count; i++)
        {
            panel.SelectHandlerForTests(i);
            Dispatcher.UIThread.RunJobs();
            Assert.Equal(i, panel.SelectedHandlerIndexForTests);
            if (panel.OperationCountForTests > 0)
            {
                panel.ExecuteSelectedForTests();
                Dispatcher.UIThread.RunJobs();
            }
        }
        Assert.True(panel.OutputCountForTests > 0,
            "walking every handler produced no output at all");
    }

    // Dispose must be safe and idempotent. The wake goroutine is joined
    // inside HandlersClose before the delegate is freed; a double Dispose
    // must not double-free the GCHandle.
    [AvaloniaFact]
    public async Task Dispose_Is_Idempotent()
    {
        var panel = await MountAsync(_bridge.DefaultPeer);
        panel.Dispose();
        panel.Dispose();
    }
}
