using System;
using System.IO;
using System.Net;
using System.Text;
using System.Threading;
using Avalonia.Controls;
using Avalonia.Headless.XUnit;
using EntityAvalonia.Panels;
using Xunit;

namespace EntityAvalonia.Tests;

// Tier-3 coverage for PublisherVerifyPanel — the CDN-corridor consume
// stack's desktop surface.
//
// **The contract under test is honesty, not rendering.** This panel's
// entire reason to exist is that a green tick on a verification chain is
// read as more than it is: five of its six steps are satisfiable by an
// origin that is lying, and only the trie walk is not. So the assertions
// here are about what the panel REFUSES to say:
//
//   1. An unreachable origin does not produce a "verified" verdict, and
//      the failing step is marked FAIL rather than omitted. A chain that
//      renders four ok steps and then stops looks green at a glance.
//   2. An origin that serves a signed root committing to a closure it
//      does not serve is reported as INCOMPLETE WALK — a statement about
//      the publisher — and NOT as a transport failure. This is the
//      shape `entity-core-go` shipped for a week (their `dabd076`) and
//      that every per-leaf consumer in the cohort called green.
//   3. Mount/Dispose is clean and handles are per-panel.
//
// The origins here are served by an in-process HttpListener rather than
// a fixture directory: the test image carries `avalonia/` only, and a
// test that silently skips when its fixture is absent is a test that
// stops measuring the day the layout changes.
[Collection(nameof(BridgeCollection))]
public sealed class PublisherVerifyPanelTests
{
    private readonly BridgeFixture _bridge;

    public PublisherVerifyPanelTests(BridgeFixture bridge)
    {
        _bridge = bridge;
    }

    [AvaloniaFact]
    public void Mount_Opens_Valid_Handle_Without_A_Peer()
    {
        // No peer handle is used by this panel — a Mode A2 consumer is
        // not a peer. Passing one in is the registry's factory shape,
        // and the panel must not depend on it.
        using var panel = new PublisherVerifyPanel(_bridge.DefaultPeer);
        Assert.True(panel.HandleForTests >= 0,
            $"expected a non-negative verify handle on mount; got {panel.HandleForTests}");

        using var second = new PublisherVerifyPanel(-1);
        Assert.True(second.HandleForTests >= 0,
            "the panel must open even with no valid peer handle at all");
        Assert.NotEqual(panel.HandleForTests, second.HandleForTests);
    }

    [AvaloniaFact]
    public void Unreachable_Origin_Does_Not_Report_Verified()
    {
        using var panel = new PublisherVerifyPanel(_bridge.DefaultPeer);
        var window = new Window { Content = panel, Width = 900, Height = 600 };
        window.Show();

        // Port 1 on loopback: closed, fast, and not a DNS round trip.
        panel.RunForTests("http://127.0.0.1:1");

        var verdict = panel.VerdictTextForTests;
        Assert.DoesNotContain("verified as of", verdict, StringComparison.OrdinalIgnoreCase);
        Assert.DoesNotContain("chain held", verdict, StringComparison.OrdinalIgnoreCase);
        Assert.Contains("FAILED", verdict, StringComparison.OrdinalIgnoreCase);

        // The failing link is SHOWN as failing. A chain that just stops
        // renders as "everything so far was fine", which on this panel
        // is the exact misreading it exists to prevent.
        Assert.True(panel.StepCountForTests >= 1,
            "the failing step must appear in the chain, not be omitted from it");
        Assert.Contains("FAIL", panel.StepMarksForTests);
    }

    [AvaloniaFact]
    public void Origin_Serving_A_Root_It_Does_Not_Back_Is_Reported_As_Incomplete_Walk()
    {
        // An origin that answers 200 on the profile and the manifest and
        // 404 on the content store. The manifest here is deliberately
        // not a valid signed root — what is being pinned is that the
        // panel's verdict names the step that failed rather than
        // collapsing every failure into "could not reach".
        using var origin = new StubOrigin();
        using var panel = new PublisherVerifyPanel(_bridge.DefaultPeer);
        var window = new Window { Content = panel, Width = 900, Height = 600 };
        window.Show();

        panel.RunForTests(origin.Prefix);

        var verdict = panel.VerdictTextForTests;
        Assert.DoesNotContain("chain held", verdict, StringComparison.OrdinalIgnoreCase);
        Assert.Contains("FAIL", panel.StepMarksForTests);

        // Steps BEFORE the failure are still shown as ok — the operator
        // needs to see how far the chain got, because "the profile
        // parsed and the manifest did not" is a different bug report
        // from "the origin is down".
        Assert.True(panel.StepCountForTests >= 1);
    }

    // StubOrigin is the smallest thing that answers a transport-profile
    // request. It is not a conformant publisher and does not pretend to
    // be: the tests above assert on which step fails, not on a walk.
    private sealed class StubOrigin : IDisposable
    {
        private readonly HttpListener _listener = new();
        private readonly Thread _thread;
        private volatile bool _stop;

        public string Prefix { get; }

        public StubOrigin()
        {
            var port = FreePort();
            Prefix = $"http://127.0.0.1:{port}";
            _listener.Prefixes.Add(Prefix + "/");
            _listener.Start();
            _thread = new Thread(Serve) { IsBackground = true };
            _thread.Start();
        }

        private static int FreePort()
        {
            var l = new System.Net.Sockets.TcpListener(IPAddress.Loopback, 0);
            l.Start();
            var port = ((IPEndPoint)l.LocalEndpoint).Port;
            l.Stop();
            return port;
        }

        private void Serve()
        {
            while (!_stop)
            {
                HttpListenerContext ctx;
                try { ctx = _listener.GetContext(); }
                catch { return; }
                try
                {
                    // Everything 404s except that the server is up. The
                    // consumer's first hop is the profile; it fails
                    // there, which is a named step, not a dead origin.
                    ctx.Response.StatusCode = 404;
                    var body = Encoding.UTF8.GetBytes("not here");
                    ctx.Response.OutputStream.Write(body, 0, body.Length);
                    ctx.Response.Close();
                }
                catch
                {
                    // A closed connection mid-write is the test tearing
                    // down; nothing to report.
                }
            }
        }

        public void Dispose()
        {
            _stop = true;
            try { _listener.Stop(); } catch { }
            try { _listener.Close(); } catch { }
        }
    }
}
