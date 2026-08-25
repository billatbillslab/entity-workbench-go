using System;
using System.Collections.Generic;
using System.Collections.ObjectModel;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Serialization;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Controls.Documents;
using Avalonia.Controls.Templates;
using Avalonia.Layout;
using Avalonia.Media;
using Avalonia.Threading;

namespace EntityAvalonia.Panels;

// BrowserPanel renders wb.BrowseModel — the consume-side **browser**:
// pin a name authority, see what it carries, go somewhere, and see the
// chain that put the bytes on screen.
//
// # What this is, next to PublisherVerifyPanel
//
// `PublisherVerifyPanel` is the inspector: you already know an origin
// and you want to know whether it is lying. It renders the chain
// INSTEAD of the site, deliberately. That surface answered one question
// and could not answer the three a person actually has — *what is out
// there*, *take me there*, *where am I*.
//
// This panel answers those, and the design problem it exists to solve is
// that **the answer looks the same whether it was verified or not**. A
// page is a page. So the chain is not behind a button and not on another
// tab: it is the right-hand column, always visible, showing the
// provenance of the bytes in the middle column and no others.
//
// # The contrast with entity-browser-rust's Site Browser, made concrete
//
// Theirs renders the pages with trust in the chrome — a banner, a state
// on the window. That is the reader's browser and it is the right shape
// for a reader. Ours puts the ten-step chain beside the page because the
// user we are building for is the person deciding whether to believe it.
//
// Two rules the model enforces and this panel must not undo:
//
//   1. A step that could not be established is shown FAILING, never
//      omitted. A rail that renders six green rows and stops looks green
//      at a glance.
//   2. No green verdict is rendered as the bare word "verified". The
//      freshness line under the rail always names a moment.
//
// Layout:
//
//   +--------------------------------------------------------------+
//   | [<] [>] [ address.......................... ] [Go]           |
//   | registry 2KBLk… @ origin · pinned by hand · as of 2026-…     |
//   +-------------+-----------------------------+------------------+
//   | REGISTRY    | Site title                  | TRUST            |
//   |  docs.…     | breadcrumbs                 |  ok registry pin |
//   |  lab.…      | -------------------------   |  ok association  |
//   |  ! ghost.…  | body                        |  ...             |
//   | SITES       |                             |  freshness bound |
//   |  demo *     |                             |                  |
//   +-------------+-----------------------------+------------------+
//
// Patterns applied (GUIDE-AVALONIA-PANEL-PATTERNS.md):
//
//   P0 breadcrumb    — PanelLog at every bridge call
//   P2 persistent    — every named control created once, never detached
//   P3′ operation-triggered wake — this panel wakes when a navigation or
//        an enumeration FINISHES, not on tree events. The single-flight
//        guard is on the START side and lives in the bridge, per
//        operation: enumerating and navigating do not block each other,
//        two navigations do.
//   P4 bounded body  — per-block split; a long page would otherwise hit
//        Skia's paint recursion (AP8)
//   P6 pinned wake   — explicit GCHandle.Alloc on the wake delegate
//
// No peer handle: a Mode A2 consumer is not a peer (§6.5.3). The
// registry factory passes one and this panel ignores it, visibly.
public sealed class BrowserPanel : UserControl, IDisposable
{
    // A published site's body is authored content and can be long. Same
    // rule as SiteViewPanel: no single SelectableTextBlock over ~500
    // inlines.
    private const int MaxInlinesPerBlock = 500;

    private readonly long _handle;

    private readonly TextBox _addressBox;
    private readonly Button _backButton;
    private readonly Button _forwardButton;
    private readonly Button _goButton;

    private readonly TextBox _registryOriginBox;
    private readonly TextBox _registryPeerBox;
    private readonly TextBox _pinTreeBox;
    private readonly TextBox _pinContentBox;
    private readonly TextBox _pinManifestBox;
    private readonly TextBox _pinLayoutBox;
    private readonly TextBox _targetOriginBox;
    private readonly Button _pinButton;
    private readonly TextBlock _registryLine;

    private readonly ListBox _nameList;
    private readonly ObservableCollection<NameRow> _names = new();
    private readonly TextBlock _namesAuthorityLine;
    private readonly TextBlock _namesNoteLine;

    private readonly ListBox _siteList;
    private readonly ObservableCollection<string> _sites = new();

    private readonly TextBlock _titleLine;
    private readonly TextBlock _crumbLine;
    private readonly StackPanel _bodyStack;
    private readonly TextBlock _errorLine;
    private readonly TextBlock _defaultedLine;

    private readonly ItemsControl _stepList;
    private readonly ObservableCollection<StepRow> _steps = new();
    private readonly TextBlock _freshnessLine;

    private Bridge.TreeWakeCallback? _wakeCallback;
    private GCHandle _wakeCallbackHandle;
    private bool _disposed;

    // _ops mirrors the bridge's completed-operation counter. A wake can
    // arrive for an operation already drawn (two can complete between two
    // dispatcher ticks), and "the Go button is enabled again" is NOT a
    // completion signal — the goroutine may not have entered the
    // operation yet when the first Render lands.
    private long _ops;

    // Test seam: counts body rebuilds so a headless test can assert the
    // page was re-rendered rather than merely re-fetched.
    public static int BodyRecreateCountForTests;

    public BrowserPanel(long peerHandle, IPanelHost? host = null)
    {
        _ = peerHandle; // see the class note: a verifying consumer is not a peer.

        var openReply = Bridge.TakeString(Bridge.BrowseOpen());
        _handle = ParseHandle(openReply);

        // Every control is constructed even on the failure path; only
        // Content is replaced. An early return leaving fields null turns
        // a later Refresh into a NullReference on the UI thread, which in
        // this runtime is a process death rather than an exception
        // (MODEL-AVALONIA-RUNTIME, the X11/Skia boundary).
        _addressBox = new TextBox
        {
            Watermark = "docs.entitychurch.org/demo/index   (or a peer-id)",
            FontFamily = new FontFamily("monospace"),
            FontSize = 13,
        };
        _addressBox.KeyDown += (_, e) =>
        {
            if (e.Key == Avalonia.Input.Key.Enter) Go();
        };
        _backButton = new Button { Content = "◀", FontSize = 13, IsEnabled = false, Padding = new Thickness(10, 2) };
        _forwardButton = new Button { Content = "▶", FontSize = 13, IsEnabled = false, Padding = new Thickness(10, 2) };
        _goButton = new Button { Content = "Go", FontSize = 13, Padding = new Thickness(14, 2) };
        _backButton.Click += (_, _) => Navigate(Bridge.BrowseBack, "back");
        _forwardButton.Click += (_, _) => Navigate(Bridge.BrowseForward, "forward");
        _goButton.Click += (_, _) => Go();

        _registryOriginBox = new TextBox
        {
            Watermark = "registry origin  (https://host or http://10.89.3.2:8099)",
            FontFamily = new FontFamily("monospace"),
            FontSize = 12,
        };
        _registryPeerBox = new TextBox
        {
            Watermark = "registry peer-id — THE PIN. For an identity-form id the pin IS the key.",
            FontFamily = new FontFamily("monospace"),
            FontSize = 12,
        };
        _pinTreeBox = MonoBox("pin tree_url_prefix (optional)");
        _pinContentBox = MonoBox("pin content_url_prefix (optional)");
        _pinManifestBox = MonoBox("pin manifest_url_prefix (optional)");
        _pinLayoutBox = MonoBox("pin content_layout, e.g. sharded-2-4 (optional)");
        _targetOriginBox = MonoBox("target origin override (optional)");
        _pinButton = new Button { Content = "Pin registry", FontSize = 12, Padding = new Thickness(12, 3) };
        _pinButton.Click += (_, _) => Pin();

        _registryLine = new TextBlock
        {
            Text = "no registry pinned — a browser must not invent a name authority",
            FontSize = 11,
            Opacity = 0.75,
            TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(0, 4, 0, 6),
        };

        _namesAuthorityLine = new TextBlock
        {
            Text = "",
            FontSize = 11,
            Opacity = 0.7,
            TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(0, 0, 0, 4),
        };
        _namesNoteLine = new TextBlock
        {
            Text = "",
            FontSize = 11,
            Foreground = Brushes.Goldenrod,
            TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(0, 0, 0, 4),
        };
        _nameList = new ListBox
        {
            ItemsSource = _names,
            FontSize = 12,
            MaxHeight = 320,
            ItemTemplate = new FuncDataTemplate<NameRow>((row, _) => BuildNameView(row), supportsRecycling: false),
        };
        _nameList.SelectionChanged += (_, _) =>
        {
            if (_nameList.SelectedItem is NameRow row && row.Committed)
            {
                _addressBox.Text = row.Name;
                Go();
            }
        };

        _siteList = new ListBox { ItemsSource = _sites, FontSize = 12, MaxHeight = 160 };
        _siteList.SelectionChanged += (_, _) =>
        {
            if (_siteList.SelectedItem is string site && !string.IsNullOrEmpty(site))
            {
                _addressBox.Text = CurrentHost() + "/" + site;
                Go();
            }
        };

        _titleLine = new TextBlock
        {
            Text = "", FontWeight = FontWeight.Bold, FontSize = 18,
            TextWrapping = TextWrapping.Wrap, Margin = new Thickness(0, 0, 0, 2),
        };
        _crumbLine = new TextBlock
        {
            Text = "", FontSize = 11, Opacity = 0.65,
            TextWrapping = TextWrapping.Wrap, Margin = new Thickness(0, 0, 0, 8),
        };
        _errorLine = new TextBlock
        {
            Text = "", FontSize = 13, Foreground = Brushes.IndianRed,
            TextWrapping = TextWrapping.Wrap, Margin = new Thickness(0, 0, 0, 8),
        };
        _defaultedLine = new TextBlock
        {
            Text = "", FontSize = 11, Opacity = 0.7,
            TextWrapping = TextWrapping.Wrap, Margin = new Thickness(0, 8, 0, 0),
        };
        _bodyStack = new StackPanel { Orientation = Orientation.Vertical };

        _stepList = new ItemsControl
        {
            ItemsSource = _steps,
            ItemTemplate = new FuncDataTemplate<StepRow>((row, _) => BuildStepView(row), supportsRecycling: false),
        };
        _freshnessLine = new TextBlock
        {
            Text = "", FontSize = 11, Opacity = 0.8,
            TextWrapping = TextWrapping.Wrap, Margin = new Thickness(0, 10, 0, 0),
        };

        if (_handle < 0)
        {
            Content = new TextBlock
            {
                Text = "browser bridge unavailable — " + openReply,
                Foreground = Brushes.IndianRed,
                TextWrapping = TextWrapping.Wrap,
                Margin = new Thickness(12),
            };
            return;
        }

        Content = BuildLayout();

        _wakeCallback = OnWakeFromGo;
        // P6: the delegate must be pinned. Go holds this pointer for the
        // life of the handle and a collected delegate is a jump into
        // freed memory, not a managed exception.
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        Bridge.TakeString(Bridge.BrowseRegisterWake(_handle,
            Marshal.GetFunctionPointerForDelegate(_wakeCallback)));
        PanelLog.Write("browse", $"Open h={_handle}");
    }

    private static TextBox MonoBox(string watermark) => new TextBox
    {
        Watermark = watermark,
        FontFamily = new FontFamily("monospace"),
        FontSize = 11,
    };

    private Control BuildLayout()
    {
        var bar = new Grid
        {
            ColumnDefinitions = new ColumnDefinitions("Auto,Auto,*,Auto"),
            Margin = new Thickness(0, 0, 0, 4),
        };
        Grid.SetColumn(_backButton, 0);
        Grid.SetColumn(_forwardButton, 1);
        Grid.SetColumn(_addressBox, 2);
        Grid.SetColumn(_goButton, 3);
        bar.Children.Add(_backButton);
        bar.Children.Add(_forwardButton);
        bar.Children.Add(_addressBox);
        bar.Children.Add(_goButton);

        var pinPanel = new StackPanel { Orientation = Orientation.Vertical, Spacing = 3 };
        pinPanel.Children.Add(_registryOriginBox);
        pinPanel.Children.Add(_registryPeerBox);
        pinPanel.Children.Add(new TextBlock
        {
            Text = "A registry that serves no transport-profile is CONFORMANT (NETWORK §6.5.4 puts " +
                   "profile distribution out-of-band in v1). Pin its layout below when there is none — " +
                   "and know that a wrong pin and a withholding origin look identical from here.",
            FontSize = 10,
            Opacity = 0.6,
            TextWrapping = TextWrapping.Wrap,
        });
        pinPanel.Children.Add(_pinTreeBox);
        pinPanel.Children.Add(_pinContentBox);
        pinPanel.Children.Add(_pinManifestBox);
        pinPanel.Children.Add(_pinLayoutBox);
        pinPanel.Children.Add(_targetOriginBox);
        pinPanel.Children.Add(_pinButton);

        var left = new StackPanel { Orientation = Orientation.Vertical, Spacing = 2 };
        left.Children.Add(new Expander
        {
            Header = "Registry pin",
            Content = pinPanel,
            IsExpanded = true,
            FontSize = 12,
        });
        left.Children.Add(_registryLine);
        left.Children.Add(SectionHeader("NAMES"));
        left.Children.Add(_namesAuthorityLine);
        left.Children.Add(_namesNoteLine);
        left.Children.Add(_nameList);
        left.Children.Add(SectionHeader("SITES HERE"));
        left.Children.Add(new TextBlock
        {
            Text = "every site this publisher's SIGNED ROOT commits to",
            FontSize = 10, Opacity = 0.55, TextWrapping = TextWrapping.Wrap,
        });
        left.Children.Add(_siteList);

        var center = new StackPanel { Orientation = Orientation.Vertical };
        center.Children.Add(_errorLine);
        center.Children.Add(_titleLine);
        center.Children.Add(_crumbLine);
        center.Children.Add(_bodyStack);
        center.Children.Add(_defaultedLine);

        var right = new StackPanel { Orientation = Orientation.Vertical, Spacing = 2 };
        right.Children.Add(SectionHeader("TRUST — for the bytes on screen"));
        right.Children.Add(new TextBlock
        {
            Text = "Every step below is satisfiable by an origin that is lying, except the walk. " +
                   "That is why this column is not a tick.",
            FontSize = 10, Opacity = 0.55, TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(0, 0, 0, 6),
        });
        right.Children.Add(_stepList);
        right.Children.Add(_freshnessLine);

        var columns = new Grid { ColumnDefinitions = new ColumnDefinitions("300,*,340") };
        var leftScroll = new ScrollViewer { Content = left, Padding = new Thickness(0, 0, 8, 0) };
        var centerScroll = new ScrollViewer { Content = center, Padding = new Thickness(8, 0) };
        var rightScroll = new ScrollViewer { Content = right, Padding = new Thickness(8, 0, 0, 0) };
        Grid.SetColumn(leftScroll, 0);
        Grid.SetColumn(centerScroll, 1);
        Grid.SetColumn(rightScroll, 2);
        columns.Children.Add(leftScroll);
        columns.Children.Add(centerScroll);
        columns.Children.Add(rightScroll);

        var root = new DockPanel { Margin = new Thickness(10) };
        DockPanel.SetDock(bar, Dock.Top);
        root.Children.Add(bar);
        root.Children.Add(columns);
        return root;
    }

    private static TextBlock SectionHeader(string text) => new TextBlock
    {
        Text = text,
        FontSize = 11,
        FontWeight = FontWeight.SemiBold,
        Opacity = 0.8,
        Margin = new Thickness(0, 10, 0, 2),
    };

    private static Control BuildNameView(NameRow row)
    {
        var stack = new StackPanel { Orientation = Orientation.Vertical };
        var head = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 6 };
        head.Children.Add(new TextBlock
        {
            Text = row.Committed ? "ok" : "!!",
            FontFamily = new FontFamily("monospace"),
            FontSize = 11,
            Foreground = row.Committed ? Brushes.MediumSeaGreen : Brushes.IndianRed,
        });
        head.Children.Add(new TextBlock { Text = row.Name, FontSize = 12 });
        stack.Children.Add(head);
        if (!string.IsNullOrEmpty(row.Note))
        {
            stack.Children.Add(new TextBlock
            {
                Text = row.Note,
                FontSize = 10,
                Opacity = row.Committed ? 0.55 : 0.95,
                Foreground = row.Committed ? null : Brushes.IndianRed,
                TextWrapping = TextWrapping.Wrap,
                Margin = new Thickness(18, 0, 0, 2),
            });
        }
        return stack;
    }

    private static Control BuildStepView(StepRow row)
    {
        var stack = new StackPanel { Orientation = Orientation.Vertical, Margin = new Thickness(0, 0, 0, 5) };
        var head = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 6 };
        head.Children.Add(new TextBlock
        {
            Text = row.Mark,
            FontFamily = new FontFamily("monospace"),
            FontSize = 11,
            Foreground = row.MarkBrush,
        });
        head.Children.Add(new TextBlock { Text = row.Name, FontSize = 12, FontWeight = FontWeight.SemiBold });
        stack.Children.Add(head);
        stack.Children.Add(new TextBlock
        {
            Text = row.Detail,
            FontFamily = new FontFamily("monospace"),
            FontSize = 10,
            Opacity = 0.7,
            TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(16, 1, 0, 0),
        });
        if (!string.IsNullOrEmpty(row.Note))
        {
            stack.Children.Add(new TextBlock
            {
                Text = row.Note,
                FontSize = 10,
                Opacity = row.NoteIsError ? 0.95 : 0.6,
                Foreground = row.NoteIsError ? Brushes.IndianRed : null,
                TextWrapping = TextWrapping.Wrap,
                Margin = new Thickness(16, 1, 0, 0),
            });
        }
        return stack;
    }

    private string CurrentHost() => _lastHost;
    private string _lastHost = "";

    private void Pin()
    {
        if (_disposed || _handle < 0) return;
        var json = JsonSerializer.Serialize(new PinConfig
        {
            Origin = _registryOriginBox.Text ?? "",
            PeerId = _registryPeerBox.Text ?? "",
            PinTree = _pinTreeBox.Text ?? "",
            PinContent = _pinContentBox.Text ?? "",
            PinManifest = _pinManifestBox.Text ?? "",
            PinLayout = _pinLayoutBox.Text ?? "",
            TargetOrigin = _targetOriginBox.Text ?? "",
        });
        var reply = Bridge.TakeString(Bridge.BrowsePin(_handle, json));
        PanelLog.Write("browse", $"Pin h={_handle} reply={reply}");

        var env = TryDecode(reply);
        if (env is { Ok: false })
        {
            _registryLine.Text = env.Error ?? "pin failed";
            _registryLine.Foreground = Brushes.IndianRed;
            return;
        }
        _registryLine.Foreground = null;
        _registryLine.Text = "pinned — nothing verified yet. A pin is a key, not a claim about an origin.";
        // Enumerating is the operation that actually checks anything.
        Bridge.TakeString(Bridge.BrowseNames(_handle));
        _namesAuthorityLine.Text = "walking the registry's signed root…";
    }

    private void Go() => GoTo(_addressBox.Text ?? "");

    private void GoTo(string addr)
    {
        if (_disposed || _handle < 0) return;
        if (string.IsNullOrWhiteSpace(addr)) return;
        _addressBox.Text = addr;
        var reply = Bridge.TakeString(Bridge.BrowseGo(_handle, addr));
        PanelLog.Write("browse", $"Go h={_handle} addr={addr} reply={reply}");
        MarkNavigating();
    }

    private void Navigate(Func<long, IntPtr> call, string what)
    {
        if (_disposed || _handle < 0) return;
        var reply = Bridge.TakeString(call(_handle));
        PanelLog.Write("browse", $"{what} h={_handle} reply={reply}");
        MarkNavigating();
    }

    private void MarkNavigating()
    {
        _goButton.IsEnabled = false;
        _errorLine.Text = "";
        _defaultedLine.Text = "";
        // The rail is cleared, never left showing the PREVIOUS page's
        // chain while the next page loads. A stale chain beside fresh
        // bytes is the one lie this panel exists to prevent.
        _steps.Clear();
        _freshnessLine.Text = "";
    }

    // OnWakeFromGo runs on a Go goroutine. Marshal to the UI thread
    // before touching a control — the visual tree is UI-thread-affine and
    // a cross-thread mutation is a crash, not an exception.
    private void OnWakeFromGo(long handle)
    {
        if (_disposed) return;
        Dispatcher.UIThread.Post(Refresh, DispatcherPriority.Background);
    }

    public void Refresh()
    {
        if (_disposed || _handle < 0) return;

        var json = Bridge.TakeString(Bridge.BrowseRender(_handle));
        BrowseEnvelope? env;
        try
        {
            env = JsonSerializer.Deserialize<BrowseEnvelope>(json, JsonOpts);
        }
        catch (Exception ex)
        {
            PanelLog.Write("browse", $"Render decode failed: {ex.Message}");
            _errorLine.Text = "(render decode failed — see the panel log)";
            _goButton.IsEnabled = true;
            return;
        }
        var v = env?.View;
        if (v == null)
        {
            _errorLine.Text = env?.Error ?? "(no view)";
            _goButton.IsEnabled = true;
            return;
        }
        _ops = env!.Ops;

        _goButton.IsEnabled = !v.Running;
        _backButton.IsEnabled = v.CanBack;
        _forwardButton.IsEnabled = v.CanForward;
        _lastHost = v.Host ?? "";

        if (!string.IsNullOrEmpty(v.Registry))
        {
            var layout = v.RegistryDiscovered
                ? "layout discovered from the origin's transport-profile"
                : "layout PINNED by hand — a wrong pin and a withholding origin look identical";
            var fresh = string.IsNullOrEmpty(v.RegistryFresh) ? "not walked yet" : "as of " + v.RegistryFresh;
            _registryLine.Foreground = null;
            _registryLine.Text = $"{v.Registry}\n@ {v.RegistryOrigin}\n{layout} · {fresh}";
        }

        _names.Clear();
        foreach (var row in v.Names ?? new List<NameDto>())
        {
            var note = !string.IsNullOrEmpty(row.Err) ? row.Err
                : row.Listed ? row.BindingHash ?? ""
                : "committed by the signed root, omitted from the served menu";
            _names.Add(new NameRow(row.Name ?? "", row.Committed, note));
        }
        _namesAuthorityLine.Text = v.NamesAuthority ?? "";
        _namesNoteLine.Text = v.NamesNote ?? "";
        _namesNoteLine.IsVisible = !string.IsNullOrEmpty(v.NamesNote);

        _sites.Clear();
        foreach (var s in v.Sites ?? new List<string>()) _sites.Add(s);

        _steps.Clear();
        foreach (var s in v.Steps ?? new List<StepDto>())
        {
            var (mark, brush) = s.Status switch
            {
                "ok" => ("ok  ", Brushes.MediumSeaGreen),
                "failed" => ("FAIL", (IBrush)Brushes.IndianRed),
                "skipped" => ("skip", Brushes.Gray),
                _ => ("..  ", (IBrush)Brushes.Gray),
            };
            // A green step shows what it PROVES; a failed or skipped one
            // shows why. Neither is optional — the note is the whole
            // reason this column is not a tick.
            var note = !string.IsNullOrEmpty(s.Err) ? s.Err : s.Proves;
            _steps.Add(new StepRow(mark, brush, s.Name ?? "", s.Detail ?? "",
                note ?? "", !string.IsNullOrEmpty(s.Err)));
        }
        _freshnessLine.Text = v.Freshness ?? "";

        _errorLine.Text = v.Err ?? "";
        if (!string.IsNullOrEmpty(v.Err))
        {
            // A refused navigation clears the page. Leaving the previous
            // one up beside a failed chain is how a user reads unverified
            // bytes as verified ones.
            _titleLine.Text = "";
            _crumbLine.Text = "";
            _bodyStack.Children.Clear();
            BodyRecreateCountForTests++;
            _addressBox.Text = _addressBox.Text;
            return;
        }

        if (!string.IsNullOrEmpty(v.Address)) _addressBox.Text = v.Address;
        var content = v.Content;
        _titleLine.Text = content?.SiteTitle ?? "";
        _crumbLine.Text = BuildCrumbs(content);
        SwapBody(content?.PageTitle ?? "", content?.BodyMarkdown ?? "");

        _defaultedLine.Text = v.SiteDefaulted
            ? $"This publisher offers {v.Sites?.Count ?? 0} sites and the address named none, so this " +
              $"is \"{v.Site}\" — first in byte order, not a front door anyone declared."
            : "";
    }

    private static string BuildCrumbs(ContentDto? content)
    {
        if (content?.Breadcrumbs == null || content.Breadcrumbs.Count == 0) return "";
        var parts = new List<string>();
        foreach (var c in content.Breadcrumbs) parts.Add(c.Label ?? "");
        return string.Join("  /  ", parts);
    }

    // SwapBody rebuilds the page body under P4's bounded-block rule.
    private void SwapBody(string pageTitle, string markdown)
    {
        _bodyStack.Children.Clear();
        if (!string.IsNullOrEmpty(pageTitle))
        {
            _bodyStack.Children.Add(new TextBlock
            {
                Text = pageTitle,
                FontWeight = FontWeight.Bold,
                FontSize = 15,
                Margin = new Thickness(0, 0, 0, 6),
            });
        }
        if (string.IsNullOrEmpty(markdown))
        {
            _bodyStack.Children.Add(new TextBlock { Text = "(empty page)", Opacity = 0.5 });
            BodyRecreateCountForTests++;
            return;
        }

        var inlines = MarkdownRenderer.BuildInlines(markdown);
        int per = 0;
        var block = NewBlock();
        for (int i = 0; i < inlines.Count; i++)
        {
            block.Inlines!.Add(inlines[i]);
            per++;
            if (inlines[i] is LineBreak && per >= MaxInlinesPerBlock)
            {
                _bodyStack.Children.Add(block);
                block = NewBlock();
                per = 0;
            }
        }
        _bodyStack.Children.Add(block);
        BodyRecreateCountForTests++;
    }

    private static SelectableTextBlock NewBlock() => new SelectableTextBlock
    {
        FontSize = 14,
        TextWrapping = TextWrapping.Wrap,
        Opacity = 0.92,
        Padding = new Thickness(2, 2, 12, 4),
    };

    private static long ParseHandle(string reply)
    {
        try
        {
            var env = JsonSerializer.Deserialize<OpenEnvelope>(reply, JsonOpts);
            if (env is { Ok: true }) return env.Handle;
        }
        catch
        {
            // fall through to the -1 sentinel
        }
        return -1;
    }

    private static BrowseEnvelope? TryDecode(string reply)
    {
        try { return JsonSerializer.Deserialize<BrowseEnvelope>(reply, JsonOpts); }
        catch { return null; }
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        if (_handle >= 0) Bridge.TakeString(Bridge.BrowseClose(_handle));
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
        _wakeCallback = null;
        PanelLog.Write("browse", $"Close h={_handle}");
    }

    // ---- test hooks (Tier 3) ----------------------------------------
    //
    // Navigation is asynchronous by construction (the bridge moves the
    // network work off the UI thread), so a test needs a way to start
    // one and settle. These poll Render rather than relying on the wake:
    // a headless test has no dispatcher loop pumping Go callbacks.

    public long HandleForTests => _handle;
    public string ErrorTextForTests => _errorLine?.Text ?? "";
    public string TitleTextForTests => _titleLine?.Text ?? "";
    public string FreshnessTextForTests => _freshnessLine?.Text ?? "";
    public string RegistryTextForTests => _registryLine?.Text ?? "";
    public string NamesAuthorityTextForTests => _namesAuthorityLine?.Text ?? "";
    public string NamesNoteTextForTests => _namesNoteLine?.Text ?? "";
    public int StepCountForTests => _steps.Count;
    public int BodyBlockCountForTests => _bodyStack?.Children.Count ?? 0;

    public IReadOnlyList<string> StepMarksForTests
    {
        get
        {
            var marks = new List<string>();
            foreach (var s in _steps) marks.Add(s.Mark.Trim());
            return marks;
        }
    }

    public IReadOnlyList<string> StepNamesForTests
    {
        get
        {
            var names = new List<string>();
            foreach (var s in _steps) names.Add(s.Name);
            return names;
        }
    }

    public IReadOnlyList<string> StepNotesForTests
    {
        get
        {
            var notes = new List<string>();
            foreach (var s in _steps) notes.Add(s.Note);
            return notes;
        }
    }

    public IReadOnlyList<string> NameRowsForTests
    {
        get
        {
            var rows = new List<string>();
            foreach (var n in _names) rows.Add((n.Committed ? "ok " : "!! ") + n.Name);
            return rows;
        }
    }

    public void PinForTests(string origin, string peerId, string tree, string content,
        string manifest, string layout, int timeoutMs = 8000)
    {
        _registryOriginBox.Text = origin;
        _registryPeerBox.Text = peerId;
        _pinTreeBox.Text = tree;
        _pinContentBox.Text = content;
        _pinManifestBox.Text = manifest;
        _pinLayoutBox.Text = layout;
        var before = _ops;
        Pin(); // Pin is synchronous; the enumeration it kicks off is not.
        Settle(before, timeoutMs);
    }

    public void GoForTests(string address, int timeoutMs = 8000)
    {
        var before = _ops;
        GoTo(address);
        Settle(before, timeoutMs);
    }

    public void BackForTests(int timeoutMs = 8000)
    {
        var before = _ops;
        Navigate(Bridge.BrowseBack, "back");
        Settle(before, timeoutMs);
    }

    // Settle waits for the bridge's completed-op counter to pass `before`.
    //
    // The button-enabled heuristic this replaced was a race in the
    // direction that makes a test pass while measuring nothing: Render
    // sets IsEnabled from `Running`, and `Running` is still false in the
    // window between the C call returning and the goroutine entering the
    // operation — so every assertion downstream ran against an empty view.
    private void Settle(long before, int timeoutMs)
    {
        var deadline = DateTime.UtcNow.AddMilliseconds(timeoutMs);
        while (DateTime.UtcNow < deadline)
        {
            Refresh();
            if (_ops > before) return;
            System.Threading.Thread.Sleep(10);
        }
        Refresh();
    }

    private sealed record StepRow(string Mark, IBrush MarkBrush, string Name, string Detail,
        string Note, bool NoteIsError);

    private sealed record NameRow(string Name, bool Committed, string Note);

    private static readonly JsonSerializerOptions JsonOpts = new()
    {
        PropertyNameCaseInsensitive = true,
    };

    private sealed class PinConfig
    {
        [JsonPropertyName("origin")] public string Origin { get; set; } = "";
        [JsonPropertyName("peer_id")] public string PeerId { get; set; } = "";
        [JsonPropertyName("pin_tree")] public string PinTree { get; set; } = "";
        [JsonPropertyName("pin_content")] public string PinContent { get; set; } = "";
        [JsonPropertyName("pin_manifest")] public string PinManifest { get; set; } = "";
        [JsonPropertyName("pin_layout")] public string PinLayout { get; set; } = "";
        [JsonPropertyName("target_origin")] public string TargetOrigin { get; set; } = "";
    }

    private sealed class OpenEnvelope
    {
        [JsonPropertyName("ok")] public bool Ok { get; set; }
        [JsonPropertyName("handle")] public long Handle { get; set; }
    }

    private sealed class BrowseEnvelope
    {
        [JsonPropertyName("ok")] public bool Ok { get; set; }
        [JsonPropertyName("error")] public string? Error { get; set; }
        [JsonPropertyName("view")] public View? View { get; set; }
        [JsonPropertyName("ops")] public long Ops { get; set; }
    }

    private sealed class View
    {
        [JsonPropertyName("Address")] public string? Address { get; set; }
        [JsonPropertyName("Host")] public string? Host { get; set; }
        [JsonPropertyName("Site")] public string? Site { get; set; }
        [JsonPropertyName("Page")] public string? Page { get; set; }
        [JsonPropertyName("Registry")] public string? Registry { get; set; }
        [JsonPropertyName("RegistryOrigin")] public string? RegistryOrigin { get; set; }
        [JsonPropertyName("RegistryDiscovered")] public bool RegistryDiscovered { get; set; }
        [JsonPropertyName("RegistryFresh")] public string? RegistryFresh { get; set; }
        [JsonPropertyName("Names")] public List<NameDto>? Names { get; set; }
        [JsonPropertyName("NamesAuthority")] public string? NamesAuthority { get; set; }
        [JsonPropertyName("NamesNote")] public string? NamesNote { get; set; }
        [JsonPropertyName("Sites")] public List<string>? Sites { get; set; }
        [JsonPropertyName("SiteDefaulted")] public bool SiteDefaulted { get; set; }
        [JsonPropertyName("Content")] public ContentDto? Content { get; set; }
        [JsonPropertyName("Steps")] public List<StepDto>? Steps { get; set; }
        [JsonPropertyName("Freshness")] public string? Freshness { get; set; }
        [JsonPropertyName("CanBack")] public bool CanBack { get; set; }
        [JsonPropertyName("CanForward")] public bool CanForward { get; set; }
        [JsonPropertyName("Running")] public bool Running { get; set; }
        [JsonPropertyName("Err")] public string? Err { get; set; }
    }

    private sealed class NameDto
    {
        [JsonPropertyName("Name")] public string? Name { get; set; }
        [JsonPropertyName("Target")] public string? Target { get; set; }
        [JsonPropertyName("BindingHash")] public string? BindingHash { get; set; }
        [JsonPropertyName("Committed")] public bool Committed { get; set; }
        [JsonPropertyName("Listed")] public bool Listed { get; set; }
        [JsonPropertyName("Err")] public string? Err { get; set; }
    }

    private sealed class ContentDto
    {
        [JsonPropertyName("SiteTitle")] public string? SiteTitle { get; set; }
        [JsonPropertyName("PageTitle")] public string? PageTitle { get; set; }
        [JsonPropertyName("BodyMarkdown")] public string? BodyMarkdown { get; set; }
        [JsonPropertyName("Breadcrumbs")] public List<CrumbDto>? Breadcrumbs { get; set; }
    }

    private sealed class CrumbDto
    {
        [JsonPropertyName("Label")] public string? Label { get; set; }
    }

    private sealed class StepDto
    {
        [JsonPropertyName("Name")] public string? Name { get; set; }
        [JsonPropertyName("Status")] public string? Status { get; set; }
        [JsonPropertyName("Detail")] public string? Detail { get; set; }
        [JsonPropertyName("Proves")] public string? Proves { get; set; }
        [JsonPropertyName("Err")] public string? Err { get; set; }
    }
}
