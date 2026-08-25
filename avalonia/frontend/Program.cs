using System;
using System.IO;
using System.Text.Json;
using Avalonia;
using Avalonia.Controls.ApplicationLifetimes;
using Avalonia.Themes.Fluent;
using Avalonia.X11;
using EntityAvalonia.Panels;

namespace EntityAvalonia;

public static class Program
{
    // Resolved config — built from argv in Main, consumed by MainWindow
    // when it formats the status line. The JSON blob itself is what
    // crosses the FFI boundary to BridgeInit.
    public static BridgeConfig Config { get; private set; } = new();
    public static string ConfigJson { get; private set; } = "";

    private const string Usage = @"Usage:
  entity-avalonia [flags]

Flags:
  --identity NAME      Use named identity from ~/.entity/identities/
                       (default: ephemeral keypair, lost on exit)
  --alias NAME         Alias for the in-process peer in the shell
                       (default: --identity name, or ""self"")
  --storage KIND       Storage backend: ""memory"" (default) or ""sqlite""
  --storage-path PATH  SQLite DB path. When --storage=sqlite and
                       --identity NAME is set, defaults to
                       ~/.entity/peers/NAME/store.db.
  --listen ADDR        TCP listener for inbound peer connections.
                       Empty (default) = no inbound listener.
  --open-access        DEV: grant wildcard caps to connecting peers.
  -h, --help           Show this message and exit.
";

    [System.STAThread]
    public static int Main(string[] args)
    {
        // FIRST thing, before argv parsing and before Avalonia exists.
        // A crash during startup is exactly as undiagnosable as one an
        // hour in, and this costs nothing when nothing goes wrong.
        CrashDiagnostics.Install();

        if (!ParseArgs(args, out var avaloniaArgs))
        {
            Console.Error.Write(Usage);
            return 2;
        }

        ConfigJson = JsonSerializer.Serialize(Config);

        if (Config.OpenAccess)
        {
            Console.Error.WriteLine(
                "entity-avalonia: WARNING — running with --open-access; all connecting " +
                "peers receive wildcard capabilities (dev only)");
        }

        return BuildAvaloniaApp().StartWithClassicDesktopLifetime(avaloniaArgs);
    }

    public static AppBuilder BuildAvaloniaApp()
    {
        var builder = AppBuilder.Configure<App>()
            .UsePlatformDetect()
            .WithInterFont()
            .LogToTrace();

        // RENDER MODE — software by default since 2026-08-19, GPU by opt-in.
        //
        // GPU rendering (mesa hardware GL) intermittently SIGSEGVs on some
        // drivers. The render itself is proven crash-free in software Skia (the
        // headless ProgramPanelStressTests rasterize a 64×64 grid 400× with no
        // GPU, and the Xvfb smoke runs use llvmpipe software GL), so the fault
        // is the driver path, not our paint code.
        //
        // **The default is inverted for release**, and this was an open product
        // call in STATUS rather than an oversight. The reasoning: we do not own
        // the faulting code and cannot fix it, we cannot reliably detect a bad
        // driver from in-process (auto-detect would mean fingerprinting mesa
        // versions, which is a guess that ages badly), and the two outcomes are
        // not symmetric — the cost of software render is frames per second on a
        // desktop app that is mostly static text and small grids, while the cost
        // of the GPU path on an affected driver is a hard crash that takes the
        // user's session with it. A slow window beats a dead one.
        //
        // Reversible in one env var, both directions:
        //   WB_GPU_RENDER=1       — opt back into hardware GL (what to try first
        //                           when someone reports sluggish rendering)
        //   WB_SOFTWARE_RENDER=1  — still honored; it is now the default, so
        //                           setting it is a no-op kept for the scripts
        //                           and docs that already pass it
        //
        // The chosen mode is announced on stderr because it is the first thing
        // a crash report needs and the last thing a reporter thinks to include.
        var gpuOptIn = !string.IsNullOrEmpty(Environment.GetEnvironmentVariable("WB_GPU_RENDER"));
        if (gpuOptIn)
        {
            Console.Error.WriteLine(
                "entity-avalonia: render mode = GPU (hardware GL, WB_GPU_RENDER set) — " +
                "if this session crashes in the driver, unset it to fall back to software");
        }
        else
        {
            builder = builder.With(new X11PlatformOptions
            {
                RenderingMode = new[] { X11RenderingMode.Software },
            });
            Console.Error.WriteLine(
                "entity-avalonia: render mode = software Skia (default; set WB_GPU_RENDER=1 for hardware GL)");
        }
        return builder;
    }

    // ParseArgs strips our flags out of args and passes the rest through
    // to Avalonia (so things like --help-avalonia or future avalonia
    // flags still work). Unknown args go through too — Avalonia ignores
    // unknown by default.
    private static bool ParseArgs(string[] args, out string[] remaining)
    {
        var passthrough = new System.Collections.Generic.List<string>();
        for (int i = 0; i < args.Length; i++)
        {
            var a = args[i];
            switch (a)
            {
                case "-h":
                case "--help":
                    remaining = passthrough.ToArray();
                    Console.Write(Usage);
                    Environment.Exit(0);
                    return true;
                case "--identity":
                    if (!TakeValue(args, ref i, a, out var ident)) { remaining = Array.Empty<string>(); return false; }
                    Config.Identity = ident;
                    break;
                case "--alias":
                    if (!TakeValue(args, ref i, a, out var alias)) { remaining = Array.Empty<string>(); return false; }
                    Config.Alias = alias;
                    break;
                case "--storage":
                    if (!TakeValue(args, ref i, a, out var sk)) { remaining = Array.Empty<string>(); return false; }
                    Config.Storage = sk;
                    break;
                case "--storage-path":
                    if (!TakeValue(args, ref i, a, out var sp)) { remaining = Array.Empty<string>(); return false; }
                    Config.StoragePath = sp;
                    break;
                case "--listen":
                    if (!TakeValue(args, ref i, a, out var ln)) { remaining = Array.Empty<string>(); return false; }
                    Config.Listen = ln;
                    break;
                case "--open-access":
                    Config.OpenAccess = true;
                    break;
                default:
                    passthrough.Add(a);
                    break;
            }
        }
        remaining = passthrough.ToArray();
        return true;
    }

    private static bool TakeValue(string[] args, ref int i, string flag, out string value)
    {
        if (i + 1 >= args.Length)
        {
            Console.Error.WriteLine($"entity-avalonia: {flag} requires a value");
            value = "";
            return false;
        }
        value = args[++i];
        return true;
    }
}

// BridgeConfig field names match the JSON tags in ../bridge/main.go
// bridgeConfig. The serializer needs explicit property names because
// the Go side reads `identity`/`alias`/etc., not the C# PascalCase.
public class BridgeConfig
{
    [System.Text.Json.Serialization.JsonPropertyName("identity")]
    public string Identity { get; set; } = "";

    [System.Text.Json.Serialization.JsonPropertyName("alias")]
    public string Alias { get; set; } = "";

    [System.Text.Json.Serialization.JsonPropertyName("storage")]
    public string Storage { get; set; } = "";

    [System.Text.Json.Serialization.JsonPropertyName("storage_path")]
    public string StoragePath { get; set; } = "";

    [System.Text.Json.Serialization.JsonPropertyName("listen")]
    public string Listen { get; set; } = "";

    [System.Text.Json.Serialization.JsonPropertyName("open_access")]
    public bool OpenAccess { get; set; }
}

public class App : Application
{
    public override void Initialize()
    {
        Styles.Add(new FluentTheme());
        // Match the project's terminal-first aesthetic (entity-shell,
        // entity-console, canvas all assume dark). Force dark so we
        // don't depend on the user's OS theme — colors should look the
        // same wherever the renderer ships.
        RequestedThemeVariant = Avalonia.Styling.ThemeVariant.Dark;

        // Register the panels available to PanelSlot dropdowns.
        // Order here = order in the slot picker menu.
        PanelRegistry.Register("detail", "Detail",
            (handle, host) => new DetailPanel(handle, host));
        PanelRegistry.Register("peer-info", "Peer Info",
            (handle, _) => new PeerInfoPanel(handle));
        PanelRegistry.Register("log-viewer", "Log Viewer",
            (handle, _) => new LogViewerPanel(handle));
        PanelRegistry.Register("markdown-view", "Markdown View",
            (handle, host) => new MarkdownViewPanel(handle, host));
        PanelRegistry.Register("markdown-files", "Markdown Files",
            (handle, host) => new MarkdownFilesPanel(handle, host));
        PanelRegistry.Register("query-browser", "Query Browser",
            (handle, host) => new QueryBrowserPanel(handle, host));
        PanelRegistry.Register("handler-browser", "Handler Browser",
            (handle, host) => new HandlerBrowserPanel(handle, host));
        PanelRegistry.Register("site-view", "Site",
            (handle, host) => new SiteViewPanel(handle, host));
        // The reader's surface is above; this is the operator's. Same
        // published bytes, opposite question — "what does it say" vs
        // "is it serving what it signed". Both shapes exist on purpose;
        // the contrast is the UX research.
        PanelRegistry.Register("publisher-verify", "Publisher Verify",
            (handle, host) => new PublisherVerifyPanel(handle, host));
        // The browser is the journey; Publisher Verify is the inspector.
        // Both stay: they answer different questions about the same
        // bytes, and an operator debugging an origin does not want a
        // page in the way.
        PanelRegistry.Register("browser", "Browser",
            (handle, host) => new BrowserPanel(handle, host));
        PanelRegistry.Register("shell", "Shell",
            (handle, host) => new ShellPanel(handle, host));
        PanelRegistry.Register("peer-connections", "Peer Connections",
            (handle, _) => new PeerConnectionsPanel(handle));
        // The generic host: ONE panel class, every program, mounted from
        // descriptors.
        //
        // The three legacy per-program panels ("Snake (compute)" / "Life
        // (compute)" / "Asteroids (compute)") were registered here alongside
        // these until 2026-08-20, so the two paths could be compared live.
        // The comparison is done: the generic path carries the display, the
        // input ports AND the program-specific status readout (POP / LEN /
        // SCORE, projected in the tree), and equality with the hard-coded
        // models is pinned by frozen state-hash vectors that no longer need
        // the legacy code to exist (programs/oracle_vectors_test.go).
        //
        // Note what is NOT here: three panel classes. The only per-program thing
        // is the string, and it is used for authoring only.
        PanelRegistry.Register("program-life", "Life (generic host)",
            (handle, _) => new ProgramPanel(handle, "life"));
        PanelRegistry.Register("program-snake", "Snake (generic host)",
            (handle, _) => new ProgramPanel(handle, "snake"));
        PanelRegistry.Register("program-asteroids", "Asteroids (generic host)",
            (handle, _) => new ProgramPanel(handle, "asteroids"));

        // The sharding floor on a screen: 64×64 Life, past the single-eval budget
        // cliff, mounting only because the host runs the static-k shard family.
        // Same ProgramPanel class as above — it never learns the program is
        // sharded (the descriptor's shard block is the host's concern). This is
        // the visual validation of the host-as-compute-kernel floor.
        PanelRegistry.Register("program-life-big", "Life 64×64 (sharded host)",
            (handle, _) => new ProgramPanel(handle, "life-big"));

        // Interactive Life: a d-pad cursor + toggle/regen/pause action buttons,
        // one key-set input port — the standard controller (controls.go),
        // mounted through the SAME generic ProgramPanel as every other program.
        PanelRegistry.Register("program-life-edit", "Life (interactive)",
            (handle, _) => new ProgramPanel(handle, "life-edit"));
    }

    public override void OnFrameworkInitializationCompleted()
    {
        // The dispatcher exists by now, so the UI-thread fault channel
        // can be hooked. Do it BEFORE constructing MainWindow — panel
        // mount is itself a place a fault can land.
        CrashDiagnostics.InstallDispatcher();

        if (ApplicationLifetime is IClassicDesktopStyleApplicationLifetime desktop)
        {
            var window = new MainWindow();
            desktop.MainWindow = window;
            // Global input breadcrumbs. Tunnelled, so a click is recorded
            // before the target handler runs — the 2026-08-21 dump died
            // in that exact window and left no trace of the click.
            CrashDiagnostics.AttachInput(window);
        }
        base.OnFrameworkInitializationCompleted();
    }
}
