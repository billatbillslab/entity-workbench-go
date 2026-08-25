using System;
using System.IO;
using System.Text.Json;
using Avalonia;
using Avalonia.Controls.ApplicationLifetimes;
using Avalonia.Themes.Fluent;
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

    public static AppBuilder BuildAvaloniaApp() =>
        AppBuilder.Configure<App>()
            .UsePlatformDetect()
            .WithInterFont()
            .LogToTrace();

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
        PanelRegistry.Register("site-view", "Site",
            (handle, host) => new SiteViewPanel(handle, host));
        PanelRegistry.Register("shell", "Shell",
            (handle, host) => new ShellPanel(handle, host));
        PanelRegistry.Register("peer-connections", "Peer Connections",
            (handle, _) => new PeerConnectionsPanel(handle));
        PanelRegistry.Register("snake", "Snake (compute)",
            (handle, _) => new SnakeGamePanel(handle));
        PanelRegistry.Register("life", "Life (compute)",
            (handle, _) => new LifeGamePanel(handle));
        PanelRegistry.Register("asteroids", "Asteroids (compute)",
            (handle, _) => new AsteroidsGamePanel(handle));

        // The generic host: ONE panel class, three programs, mounted from
        // descriptors. Registered ALONGSIDE the three legacy panels above so the
        // two paths can be compared live before the legacy ones are removed —
        // the legacy models are also the oracle the mounted programs are pinned
        // against (workbench/program_host_test.go), so they earn their keep
        // until the comparison is done.
        //
        // Note what is NOT here: three panel classes. The only per-program thing
        // is the string, and it is used for authoring only.
        PanelRegistry.Register("program-life", "Life (generic host)",
            (handle, _) => new ProgramPanel(handle, "life"));
        PanelRegistry.Register("program-snake", "Snake (generic host)",
            (handle, _) => new ProgramPanel(handle, "snake"));
        PanelRegistry.Register("program-asteroids", "Asteroids (generic host)",
            (handle, _) => new ProgramPanel(handle, "asteroids"));
    }

    public override void OnFrameworkInitializationCompleted()
    {
        if (ApplicationLifetime is IClassicDesktopStyleApplicationLifetime desktop)
        {
            desktop.MainWindow = new MainWindow();
        }
        base.OnFrameworkInitializationCompleted();
    }
}
