using System;
using System.IO;
using System.Linq;
using System.Net;
using System.Text;
using System.Threading;
using Avalonia.Controls;
using Avalonia.Headless.XUnit;
using EntityAvalonia.Panels;
using Xunit;

namespace EntityAvalonia.Tests;

// Tier-3 coverage for BrowserPanel — the consume-side browser.
//
// **These run against another implementation's real bytes.** The build
// context COPYs the whole workbench tree, so
// `fetch/testdata/crossimpl-rust-federation` — `entity-browser-rust`'s
// frozen `make federation` emission: a static registry signing four
// names and the four domains they point at — is present in the tester
// stage. The panel is driven end to end over it: pin, walk, resolve,
// verify, render a page.
//
// **The contract under test is honesty, not rendering.** A page looks
// identical whether ten checks passed or none did, so every assertion
// here is about what the panel is obliged to SAY or REFUSE to say:
//
//   1. Pinning verifies nothing, and the panel says so. "Pinned" reads
//      like "checked".
//   2. The name list names its own provenance (the walk), and a name the
//      origin advertises without a signed binding is shown MARKED, not
//      dropped and not offered.
//   3. A substituted by-name pointer is refused, the refusal names the
//      binding that actually answered, and the page is CLEARED — leaving
//      the previous page up beside a failed chain is how unverified
//      bytes get read as verified ones.
//   4. A green chain never renders the bare word "verified"; the
//      freshness line names a moment.
//
// If the fixture is missing these tests FAIL rather than skip. A test
// that quietly skips when its fixture moves is a test that stopped
// measuring and did not mention it.
[Collection(nameof(BridgeCollection))]
public sealed class BrowserPanelTests
{
    private const string RegistryPeer = "2KBLkCxvkgobuauPA6zPfKarpuRRnnWHL98n8Gv1GNmybr";
    private const string DocsPeer = "2KFRBJ9feEPCZZiNaCEKsCAVkGkp1htWZk9a8jz5n5D2sS";

    private readonly BridgeFixture _bridge;

    public BrowserPanelTests(BridgeFixture bridge)
    {
        _bridge = bridge;
    }

    // FixtureRoot resolves the frozen federation. The tester stage's
    // working directory is the test project; the fixture sits three
    // levels up under fetch/.
    private static string FixtureRoot()
    {
        var candidates = new[]
        {
            Path.Combine("..", "..", "..", "..", "..", "fetch", "testdata", "crossimpl-rust-federation"),
            Path.Combine("/src", "entity-workbench-go", "fetch", "testdata", "crossimpl-rust-federation"),
        };
        foreach (var c in candidates)
        {
            if (Directory.Exists(c)) return Path.GetFullPath(c);
        }
        throw new DirectoryNotFoundException(
            "the frozen crossimpl-rust-federation fixture is not reachable from the test image. " +
            "It is COPY'd with the workbench tree; if the layout moved, fix the path here rather " +
            "than skipping — these tests are the only GUI-side measurement of the naming hop. " +
            "Looked in: " + string.Join(", ", candidates.Select(Path.GetFullPath)));
    }

    private static BrowserPanel Pinned(FixtureOrigin origin, long peer)
    {
        var panel = new BrowserPanel(peer);
        var window = new Window { Content = panel, Width = 1200, Height = 700 };
        window.Show();
        panel.PinForTests(
            origin.Prefix,
            RegistryPeer,
            "/registry/" + RegistryPeer,
            "/registry/content",
            "/registry/" + RegistryPeer + "/system/peer/published-root",
            "sharded-2-4");
        return panel;
    }

    [AvaloniaFact]
    public void Mount_Opens_Valid_Handle_Without_A_Peer()
    {
        // A Mode A2 consumer is not a peer. The registry factory passes a
        // peer handle; the panel must not depend on it.
        using var panel = new BrowserPanel(_bridge.DefaultPeer);
        Assert.True(panel.HandleForTests >= 0,
            $"expected a non-negative browse handle on mount; got {panel.HandleForTests}");

        using var second = new BrowserPanel(-1);
        Assert.True(second.HandleForTests >= 0,
            "the panel must open even with no valid peer handle at all");
        Assert.NotEqual(panel.HandleForTests, second.HandleForTests);
    }

    [AvaloniaFact]
    public void Pinning_Says_That_It_Verified_Nothing()
    {
        using var origin = new FixtureOrigin(FixtureRoot());
        using var panel = Pinned(origin, _bridge.DefaultPeer);

        var line = panel.RegistryTextForTests;
        Assert.Contains(RegistryPeer, line, StringComparison.Ordinal);
        Assert.Contains("PINNED by hand", line, StringComparison.OrdinalIgnoreCase);
        // The registry serves no transport-profile — conformant per
        // NETWORK §6.5.4 — so the panel must not imply discovery.
        Assert.DoesNotContain("discovered", line, StringComparison.OrdinalIgnoreCase);
    }

    [AvaloniaFact]
    public void Name_List_Comes_From_The_Walk_And_Says_So()
    {
        using var origin = new FixtureOrigin(FixtureRoot());
        using var panel = Pinned(origin, _bridge.DefaultPeer);

        var rows = panel.NameRowsForTests;
        Assert.Equal(4, rows.Count);
        Assert.Contains(rows, r => r.Contains("docs.entitychurch.org", StringComparison.Ordinal));
        Assert.All(rows, r => Assert.StartsWith("ok ", r, StringComparison.Ordinal));

        Assert.Contains("walk", panel.NamesAuthorityTextForTests, StringComparison.OrdinalIgnoreCase);
        Assert.Equal("", panel.NamesNoteTextForTests);
    }

    [AvaloniaFact]
    public void An_Advertised_Only_Name_Is_Marked_Not_Dropped_And_Not_Offered()
    {
        // The origin invents a fifth name in the served listing. It signs
        // nothing, so the walk does not carry it. Dropping the row hides
        // a disagreement the operator should see; offering it promises
        // something that cannot resolve.
        using var origin = new FixtureOrigin(FixtureRoot())
        {
            ListingOverride = "entitychurch.org\ndocs.entitychurch.org\nlab.entitychurch.org\n" +
                              "protocol.entitychurch.org\nghost.entitychurch.org\n",
        };
        using var panel = Pinned(origin, _bridge.DefaultPeer);

        var rows = panel.NameRowsForTests;
        Assert.Contains(rows, r => r.Contains("ghost.entitychurch.org", StringComparison.Ordinal));
        Assert.Contains(rows, r => r.StartsWith("!! ", StringComparison.Ordinal));
        Assert.NotEqual("", panel.NamesNoteTextForTests);
    }

    [AvaloniaFact]
    public void Open_By_Name_Renders_A_Page_With_The_Whole_Chain()
    {
        using var origin = new FixtureOrigin(FixtureRoot());
        using var panel = Pinned(origin, _bridge.DefaultPeer);

        panel.GoForTests("docs.entitychurch.org");

        // A freshness failure here means the fixture's 30-day bindings
        // have expired. That is not a regression in this panel — it is
        // the fixture asking to be re-cut, and the message says so
        // rather than leaving a future reader with a mystery.
        var names = panel.StepNamesForTests;
        var marks = panel.StepMarksForTests;
        for (int i = 0; i < names.Count; i++)
        {
            if (names[i] == "binding freshness" && marks[i] == "FAIL")
            {
                Assert.Fail("the frozen federation's bindings have expired — re-cut " +
                            "fetch/testdata/crossimpl-rust-federation from a fresh " +
                            "`make federation` in entity-browser-rust and update its README.");
            }
        }

        Assert.Equal("", panel.ErrorTextForTests);
        Assert.True(panel.BodyBlockCountForTests > 0, "no page body was rendered");

        foreach (var step in new[]
                 {
                     "registry pin", "name lookup", "binding signature", "association",
                     "revocation", "binding freshness", "transport", "target root",
                     "target walk", "page",
                 })
        {
            Assert.Contains(step, names);
        }
        Assert.DoesNotContain("FAIL", marks);

        // Every step carries what it proves — that field is the whole
        // design, not decoration.
        Assert.All(panel.StepNotesForTests, n => Assert.False(string.IsNullOrWhiteSpace(n)));

        // The green verdict is scoped to a moment and never stated bare.
        var freshness = panel.FreshnessTextForTests;
        Assert.Contains("as of", freshness, StringComparison.OrdinalIgnoreCase);
    }

    [AvaloniaFact]
    public void A_Substituted_Binding_Is_Refused_And_The_Page_Is_Cleared()
    {
        // The origin repoints one by-name file at a binding the registry
        // legitimately signed for a DIFFERENT name. Signature, signer,
        // revocation and expiry all pass; only the association fails.
        //
        // **And it has to reach the pointer path to be reachable at all.**
        // When an enumeration is loaded the browser resolves out of the
        // SIGNED key set, where there is no host-chosen association to
        // substitute — the attack is not detected there, it is
        // impossible. So this origin also refuses to serve its
        // published-root, which is what a registry too large or too slow
        // to walk looks like: the browser falls back to §6a.4's floor,
        // and the floor is where the association check earns its keep.
        var root = FixtureRoot();
        using var origin = new FixtureOrigin(root)
        {
            SubstituteByName = ("docs.entitychurch.org", "lab.entitychurch.org"),
            WithholdManifest = true,
        };
        using var panel = Pinned(origin, _bridge.DefaultPeer);

        panel.GoForTests("docs.entitychurch.org");

        Assert.NotEqual("", panel.ErrorTextForTests);
        Assert.Contains("lab.entitychurch.org", panel.ErrorTextForTests, StringComparison.Ordinal);
        Assert.Contains("FAIL", panel.StepMarksForTests);

        // The steps before the break are still shown — a rail that
        // renders nothing on failure teaches nothing about where it broke.
        Assert.Contains("registry pin", panel.StepNamesForTests);

        // And no page is left on screen next to a failed chain.
        Assert.Equal("", panel.TitleTextForTests);
        Assert.Equal(0, panel.BodyBlockCountForTests);
    }

    [AvaloniaFact]
    public void The_Walk_Path_Is_Immune_To_The_Same_Substitution()
    {
        // The mirror of the test above, and the stronger property: with
        // the enumeration loaded, the same repointed file changes
        // nothing, because the binding hash came out of the signed root.
        // §6a.3a's asymmetry, at the panel.
        var root = FixtureRoot();
        using var origin = new FixtureOrigin(root)
        {
            SubstituteByName = ("docs.entitychurch.org", "lab.entitychurch.org"),
        };
        using var panel = Pinned(origin, _bridge.DefaultPeer);

        panel.GoForTests("docs.entitychurch.org");

        Assert.Equal("", panel.ErrorTextForTests);
        Assert.DoesNotContain("FAIL", panel.StepMarksForTests);
        Assert.True(panel.BodyBlockCountForTests > 0);
    }

    [AvaloniaFact]
    public void An_Unreachable_Origin_Never_Reports_A_Green_Chain()
    {
        using var panel = new BrowserPanel(_bridge.DefaultPeer);
        var window = new Window { Content = panel, Width = 1000, Height = 600 };
        window.Show();

        // Port 1 on loopback: closed, fast, and not a DNS round trip.
        panel.PinForTests("http://127.0.0.1:1", RegistryPeer,
            "/registry/" + RegistryPeer, "/registry/content",
            "/registry/" + RegistryPeer + "/system/peer/published-root", "sharded-2-4");
        panel.GoForTests("docs.entitychurch.org");

        Assert.NotEqual("", panel.ErrorTextForTests);
        Assert.DoesNotContain("as of", panel.FreshnessTextForTests, StringComparison.OrdinalIgnoreCase);
        Assert.Contains("FAIL", panel.StepMarksForTests);
        Assert.Equal(0, panel.BodyBlockCountForTests);
    }

    // FixtureOrigin serves the frozen federation from disk, with hooks a
    // test uses to build a DISHONEST origin without editing a byte on
    // disk. The bytes stay another implementation's bytes; only what the
    // host chooses to answer with changes, which is exactly the power
    // EXTENSION-REGISTRY §6a.1a gives the party serving them.
    private sealed class FixtureOrigin : IDisposable
    {
        private readonly HttpListener _listener = new();
        private readonly Thread _thread;
        private readonly string _root;
        private volatile bool _stop;

        public string Prefix { get; }

        // ListingOverride replaces the served by-name listing.
        public string? ListingOverride { get; set; }

        // SubstituteByName answers the first name's by-name pointer with
        // the second name's pointer bytes.
        public (string asked, string served)? SubstituteByName { get; set; }

        // WithholdManifest 404s the registry's published-root, which is
        // what a registry that cannot be walked looks like to a
        // consumer — and is how a test reaches the §6a.4 pointer floor.
        public bool WithholdManifest { get; set; }

        public FixtureOrigin(string root)
        {
            _root = root;
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
                    var path = ctx.Request.Url?.AbsolutePath ?? "/";

                    if (WithholdManifest && path.EndsWith("/system/peer/published-root", StringComparison.Ordinal))
                    {
                        ctx.Response.StatusCode = 404;
                        ctx.Response.Close();
                        continue;
                    }

                    if (ListingOverride != null &&
                        path.EndsWith("/system/registry/binding/by-name.list", StringComparison.Ordinal))
                    {
                        Write(ctx, Encoding.UTF8.GetBytes(ListingOverride));
                        continue;
                    }

                    if (SubstituteByName is { } sub &&
                        path.EndsWith("/by-name/" + sub.asked + ".bin", StringComparison.Ordinal))
                    {
                        var swapped = Path.Combine(_root, "registry", RegistryPeer,
                            "system", "registry", "binding", "by-name", sub.served + ".bin");
                        Write(ctx, File.ReadAllBytes(swapped));
                        continue;
                    }

                    var file = Path.Combine(_root, path.TrimStart('/').Replace('/', Path.DirectorySeparatorChar));
                    if (File.Exists(file))
                    {
                        Write(ctx, File.ReadAllBytes(file));
                    }
                    else
                    {
                        ctx.Response.StatusCode = 404;
                        ctx.Response.Close();
                    }
                }
                catch
                {
                    try { ctx.Response.Abort(); } catch { /* the client went away */ }
                }
            }
        }

        private static void Write(HttpListenerContext ctx, byte[] body)
        {
            ctx.Response.StatusCode = 200;
            ctx.Response.ContentType = "application/octet-stream";
            ctx.Response.ContentLength64 = body.Length;
            ctx.Response.OutputStream.Write(body, 0, body.Length);
            ctx.Response.Close();
        }

        public void Dispose()
        {
            _stop = true;
            try { _listener.Stop(); } catch { /* already stopped */ }
            try { _listener.Close(); } catch { /* already closed */ }
        }
    }
}
