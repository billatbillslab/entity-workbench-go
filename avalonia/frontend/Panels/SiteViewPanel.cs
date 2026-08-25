using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Serialization;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Controls.Documents;
using Avalonia.Layout;
using Avalonia.Media;
using Avalonia.Threading;

namespace EntityAvalonia.Panels;

// SiteViewPanel renders wb.SiteModel — the SITE convention's
// read-projection (app/site-manifest + app/site-page, v0.5). First
// new-feature exercise of the renderer-agnostic substrate (D18); the
// console renderer carries a stub against the same model so a model
// contract change breaks BOTH renderers.
//
// Layout:
//
//   +--------------------------------------------------+
//   | < back  | Site title  | [Nav links]              |
//   +--------------------------------------------------+
//   | Sidebar      | breadcrumbs                       |
//   | (sections)   | -------------------------------   |
//   |              | Body (rendered markdown)          |
//   +--------------------------------------------------+
//
// Patterns applied (from GUIDE-AVALONIA-PANEL-PATTERNS.md):
//
//   P0 breadcrumb — PanelLog at every long-running op
//   P2 persistent containers — every named control is created once
//   P3 wake debounce — single-flight + 150ms DispatcherTimer
//   P4 bounded body — per-block split (b); a site page that grows
//      unbounded would hit Skia's paint recursion otherwise (AP8)
//   P6 pinned wake — explicit GCHandle.Alloc on the wake delegate
//
// P1 (adaptive emit) is deferred: site pages are curated content
// (manifest-listed; not arbitrary user-imported markdown), so the
// payload is small in practice. If a >5K-inline page surfaces, lift
// the EmitBatch pipeline from MarkdownViewPanel.SwapBody (~150 lines).
//
// P5 (differential update) — sidebar/nav are small lists rebuilt
// in-place on render. No diff machinery; just clear + Add into the
// persistent stacks. If sidebar grows past ~50 rows or nav rebuild
// dominates frame time, lift the in-place-replace shape from
// MarkdownFilesPanel.RowsPathSetMatches.
public sealed class SiteViewPanel : UserControl, IDisposable
{
    private readonly long _peerHandle;
    private readonly long _handle;
    private readonly string _siteID;

    // Persistent containers — created in the constructor's happy
    // path, never detached. The `= null!` suppression is for the
    // _handle<0 error-return path that doesn't reach the assignments;
    // none of these are dereferenced when _handle<0 (every method
    // bails on the handle check first).
    private readonly StackPanel _navBar = null!;
    private readonly Button _backButton = null!;
    private readonly TextBlock _siteTitleText = null!;
    private readonly StackPanel _navLinks = null!;

    private readonly StackPanel _sidebar = null!;
    private readonly StackPanel _breadcrumbs = null!;
    private readonly ScrollViewer _bodyScroll = null!;
    private readonly StackPanel _bodyStack = null!;
    private readonly TextBlock _placeholder = null!;

    // P6 — explicit GCHandle root for the wake delegate. The field
    // alone isn't enough; GCHandle.Alloc is belt-and-suspenders.
    private Bridge.TreeWakeCallback? _wakeCallback;
    private GCHandle _wakeCallbackHandle;

    // P3 — single-flight (do not queue multiple renders per tick)
    // plus a short DispatcherTimer that coalesces a burst into one.
    private bool _renderQueued;
    private DispatcherTimer? _wakeTimer;
    private static readonly TimeSpan WakeDebounce = TimeSpan.FromMilliseconds(150);

    // Bounded body — keep no SelectableTextBlock above this inline
    // count (AP8: Skia's paint recursion grows with inline depth).
    private const int MaxInlinesPerBlock = 500;

    private bool _disposed;

    // Test surface: how many times the body has been (re)materialized.
    internal int BodyRecreateCountForTests { get; private set; }
    // Test surface: bridge-side render-call counter under stress.
    internal int RenderCallCountForTests { get; private set; }
    // Test surface: drive Navigate without going through a click handler.
    internal void NavigateForTests(string target) => NavigateTo(target);
    // Test surface: drive GoBack without going through the back button.
    internal void GoBackForTests()
    {
        if (_handle < 0) return;
        Bridge.TakeString(Bridge.SiteGoBack(_handle));
    }
    // Test surface: last-rendered output (title + breadcrumbs).
    internal string SiteTitleForTests => _siteTitleText?.Text ?? "";
    internal int BreadcrumbCountForTests => _breadcrumbs?.Children.Count ?? 0;
    internal int SidebarCountForTests => _sidebar?.Children.Count ?? 0;

    public SiteViewPanel(long peerHandle, IPanelHost? host)
        : this(peerHandle, host, siteID: "demo") { }

    public SiteViewPanel(long peerHandle, IPanelHost? host, string siteID)
    {
        _peerHandle = peerHandle;
        _siteID = string.IsNullOrEmpty(siteID) ? "demo" : siteID;
        PanelLog.Write("site-view", $"Mount peerHandle={peerHandle} siteID={_siteID}");

        // Peer-id "" → bridge substitutes the bound peer.
        var openReply = Bridge.TakeString(Bridge.SiteOpen(peerHandle, "", _siteID));
        _handle = ParseHandle(openReply);
        if (_handle < 0)
        {
            Content = new SelectableTextBlock
            {
                Text = $"site-view open failed: {openReply}",
                Foreground = Brushes.IndianRed,
                Margin = new Thickness(12),
                FontSize = 14,
            };
            PanelLog.Write("site-view", $"Mount open-failed reply={openReply}");
            return;
        }

        // --- Nav bar (persistent) ----
        _backButton = new Button
        {
            Content = "<",
            Padding = new Thickness(8, 2),
            Margin = new Thickness(0, 0, 8, 0),
            IsEnabled = false,
        };
        _backButton.Click += OnBackClicked;
        _siteTitleText = new TextBlock
        {
            Text = "",
            FontWeight = FontWeight.SemiBold,
            FontSize = 15,
            VerticalAlignment = VerticalAlignment.Center,
            Margin = new Thickness(0, 0, 16, 0),
        };
        _navLinks = new StackPanel
        {
            Orientation = Orientation.Horizontal,
        };
        _navBar = new StackPanel
        {
            Orientation = Orientation.Horizontal,
            Margin = new Thickness(10, 8),
        };
        _navBar.Children.Add(_backButton);
        _navBar.Children.Add(_siteTitleText);
        _navBar.Children.Add(_navLinks);

        // --- Sidebar (persistent) ----
        _sidebar = new StackPanel
        {
            Orientation = Orientation.Vertical,
            Margin = new Thickness(10, 8),
        };
        var sidebarScroll = new ScrollViewer
        {
            Content = _sidebar,
            HorizontalScrollBarVisibility = Avalonia.Controls.Primitives.ScrollBarVisibility.Disabled,
            VerticalScrollBarVisibility = Avalonia.Controls.Primitives.ScrollBarVisibility.Auto,
            Width = 200,
        };

        // --- Main column ----
        _breadcrumbs = new StackPanel
        {
            Orientation = Orientation.Horizontal,
            Margin = new Thickness(10, 6),
        };
        _bodyStack = new StackPanel
        {
            Orientation = Orientation.Vertical,
            Margin = new Thickness(10, 4),
        };
        _bodyScroll = new ScrollViewer
        {
            Content = _bodyStack,
            HorizontalScrollBarVisibility = Avalonia.Controls.Primitives.ScrollBarVisibility.Disabled,
            VerticalScrollBarVisibility = Avalonia.Controls.Primitives.ScrollBarVisibility.Auto,
        };
        _placeholder = new TextBlock
        {
            Text = "(loading site…)",
            Opacity = 0.5,
            Margin = new Thickness(12),
            FontSize = 13,
        };
        _bodyStack.Children.Add(_placeholder);

        var mainColumn = new DockPanel
        {
            LastChildFill = true,
        };
        DockPanel.SetDock(_breadcrumbs, Dock.Top);
        mainColumn.Children.Add(_breadcrumbs);
        mainColumn.Children.Add(_bodyScroll);

        var bodyRow = new Grid
        {
            ColumnDefinitions = new ColumnDefinitions("Auto,*"),
        };
        Grid.SetColumn(sidebarScroll, 0);
        Grid.SetColumn(mainColumn, 1);
        bodyRow.Children.Add(sidebarScroll);
        bodyRow.Children.Add(mainColumn);

        var root = new Grid
        {
            RowDefinitions = new RowDefinitions("Auto,*"),
        };
        Grid.SetRow(_navBar, 0);
        Grid.SetRow(bodyRow, 1);
        root.Children.Add(_navBar);
        root.Children.Add(bodyRow);
        Content = root;

        // P6 — pin the wake delegate.
        _wakeCallback = OnWakeFromGo;
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        var cbPtr = Marshal.GetFunctionPointerForDelegate(_wakeCallback);
        Bridge.TakeString(Bridge.SiteRegisterWake(_handle, cbPtr));
        PanelLog.Write("site-view", $"Mount registered wake h={_handle}");

        // First render — synchronous; subsequent renders are debounced.
        RerenderFromBridge();
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        PanelLog.Write("site-view", $"Dispose h={_handle}");
        if (_wakeTimer != null)
        {
            _wakeTimer.Stop();
            _wakeTimer.Tick -= OnWakeTimerTick;
            _wakeTimer = null;
        }
        if (_handle >= 0)
        {
            Bridge.SiteClose(_handle);
        }
        _wakeCallback = null;
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
    }

    // -- Wake path -----------------------------------------------------

    private void OnWakeFromGo(long handle)
    {
        if (_disposed) return;
        // Single-flight + time-debounce. A burst of wakes during a
        // resolver-side cross-peer fetch should collapse to one render.
        if (_renderQueued) return;
        _renderQueued = true;
        Dispatcher.UIThread.Post(() =>
        {
            _renderQueued = false;
            if (_disposed) return;
            if (_wakeTimer == null)
            {
                _wakeTimer = new DispatcherTimer { Interval = WakeDebounce };
                _wakeTimer.Tick += OnWakeTimerTick;
            }
            _wakeTimer.Stop();
            _wakeTimer.Start();
        });
    }

    private void OnWakeTimerTick(object? sender, EventArgs e)
    {
        _wakeTimer?.Stop();
        if (_disposed) return;
        RerenderFromBridge();
    }

    // -- Render --------------------------------------------------------

    private void RerenderFromBridge()
    {
        if (_handle < 0) return;
        RenderCallCountForTests++;
        PanelLog.Write("site-view", $"Render h={_handle}");
        var reply = Bridge.TakeString(Bridge.SiteRender(_handle));
        SiteRenderOutputDto? dto = null;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                _placeholder.Text = reply;
                _placeholder.Foreground = Brushes.IndianRed;
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<SiteRenderOutputDto>();
        }
        catch (Exception ex)
        {
            _placeholder.Text = $"site-view render parse failed: {ex.Message}";
            _placeholder.Foreground = Brushes.OrangeRed;
            return;
        }
        if (dto == null)
        {
            _placeholder.Text = "(empty render)";
            return;
        }

        // Top-line state: site title + back button.
        _siteTitleText.Text = string.IsNullOrEmpty(dto.SiteTitle) ? "(site)" : dto.SiteTitle;
        _backButton.IsEnabled = dto.CanGoBack;

        // Nav links — clear + Add into the persistent stack.
        _navLinks.Children.Clear();
        if (dto.Nav != null)
        {
            foreach (var nl in dto.Nav)
            {
                _navLinks.Children.Add(BuildNavLink(nl));
            }
        }

        // Sidebar — clear + Add. Empty list = single-pane fallback.
        _sidebar.Children.Clear();
        if (dto.Sidebar != null)
        {
            foreach (var s in dto.Sidebar)
            {
                _sidebar.Children.Add(BuildSidebarLink(s));
            }
        }

        // Breadcrumbs.
        _breadcrumbs.Children.Clear();
        if (dto.Breadcrumbs != null)
        {
            for (int i = 0; i < dto.Breadcrumbs.Count; i++)
            {
                if (i > 0)
                {
                    _breadcrumbs.Children.Add(new TextBlock
                    {
                        Text = " > ",
                        Opacity = 0.5,
                        Margin = new Thickness(4, 0),
                    });
                }
                _breadcrumbs.Children.Add(BuildCrumb(dto.Breadcrumbs[i]));
            }
        }

        // Error / loading / body.
        if (!string.IsNullOrEmpty(dto.Error))
        {
            _bodyStack.Children.Clear();
            _bodyStack.Children.Add(new TextBlock
            {
                Text = $"error: {dto.Error}",
                Foreground = Brushes.IndianRed,
                Margin = new Thickness(12),
                FontSize = 14,
            });
            return;
        }
        if (dto.Loading)
        {
            _bodyStack.Children.Clear();
            _bodyStack.Children.Add(new TextBlock
            {
                Text = "(loading…)",
                Opacity = 0.6,
                Margin = new Thickness(12),
            });
            return;
        }

        SwapBody(dto.PageTitle, dto.BodyMarkdown ?? "");
    }

    private Button BuildNavLink(NavLinkDto nl)
    {
        var btn = new Button
        {
            Content = nl.Label,
            Padding = new Thickness(8, 2),
            Margin = new Thickness(4, 0),
            FontWeight = nl.Active ? FontWeight.SemiBold : FontWeight.Normal,
            Opacity = nl.Active ? 1.0 : 0.85,
        };
        if (nl.Kind == "section-header" || string.IsNullOrEmpty(nl.Target))
        {
            btn.IsEnabled = false;
        }
        else
        {
            btn.Click += (s, e) => NavigateTo(nl.Target);
        }
        return btn;
    }

    private Control BuildSidebarLink(SectionLinkDto s)
    {
        var indent = new Thickness(8 + s.Depth * 12, 2, 4, 2);
        var btn = new Button
        {
            Content = s.Label,
            Padding = indent,
            HorizontalAlignment = HorizontalAlignment.Stretch,
            HorizontalContentAlignment = HorizontalAlignment.Left,
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
            FontWeight = s.Active ? FontWeight.SemiBold : FontWeight.Normal,
            Opacity = s.Active ? 1.0 : 0.8,
        };
        btn.Click += (sender, e) => NavigateTo(s.Target);
        return btn;
    }

    private Control BuildCrumb(CrumbDto c)
    {
        if (string.IsNullOrEmpty(c.Target))
        {
            // "You are here" — plain label.
            return new TextBlock
            {
                Text = c.Label,
                FontWeight = FontWeight.SemiBold,
                Opacity = 0.9,
            };
        }
        var btn = new Button
        {
            Content = c.Label,
            Padding = new Thickness(0),
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
            Foreground = new SolidColorBrush(Color.FromRgb(0x7e, 0xc5, 0xff)),
        };
        btn.Click += (s, e) => NavigateTo(c.Target);
        return btn;
    }

    private void NavigateTo(string target)
    {
        if (_handle < 0 || string.IsNullOrEmpty(target)) return;
        PanelLog.Write("site-view", $"Navigate h={_handle} target={target}");
        Bridge.TakeString(Bridge.SiteNavigate(_handle, target));
        PanelLog.Write("site-view", $"Navigate-returned h={_handle}");
        // Wake fires asynchronously via OnChange; no need to re-render
        // here. The wake path coalesces rapid navigation through the
        // single-flight + DispatcherTimer guard.
    }

    private void OnBackClicked(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        if (_handle < 0) return;
        PanelLog.Write("site-view", $"GoBack h={_handle}");
        Bridge.TakeString(Bridge.SiteGoBack(_handle));
    }

    // -- Body swap (P4 per-block cap) ----------------------------------

    private void SwapBody(string pageTitle, string markdown)
    {
        PanelLog.Write("site-view",
            $"SwapBody h={_handle} title={pageTitle} bytes={markdown.Length}");

        _bodyStack.Children.Clear();

        if (!string.IsNullOrEmpty(pageTitle))
        {
            _bodyStack.Children.Add(new TextBlock
            {
                Text = pageTitle,
                FontWeight = FontWeight.Bold,
                FontSize = 18,
                Margin = new Thickness(0, 0, 0, 8),
            });
        }

        if (string.IsNullOrEmpty(markdown))
        {
            _bodyStack.Children.Add(new TextBlock
            {
                Text = "(empty page)",
                Opacity = 0.5,
            });
            BodyRecreateCountForTests++;
            return;
        }

        var inlines = MarkdownRenderer.BuildInlines(markdown);

        // P4 per-block split (b) — keep no block above MaxInlinesPerBlock.
        // We never materialize a single SelectableTextBlock with more
        // than ~500 inlines (AP8: Skia paint recursion).
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
        PanelLog.Write("site-view", $"SwapBody-done h={_handle} blocks={_bodyStack.Children.Count}");
    }

    private static SelectableTextBlock NewBlock() => new SelectableTextBlock
    {
        FontSize = 14,
        TextWrapping = TextWrapping.Wrap,
        Opacity = 0.92,
        Padding = new Thickness(4, 2, 12, 4),
    };

    private static long ParseHandle(string envelope)
    {
        try
        {
            using var doc = JsonDocument.Parse(envelope);
            var root = doc.RootElement;
            if (root.TryGetProperty("ok", out var ok) && ok.GetBoolean()
                && root.TryGetProperty("handle", out var h))
            {
                return h.GetInt64();
            }
        }
        catch { }
        return -1;
    }

    // -- DTOs ---------------------------------------------------------
    //
    // Mirrors wb.SiteRenderOutput's JSON serialization. Field names
    // match Go's exported names (Title-cased) since encoding/json
    // serializes by Go-field-name without a tag.

    private sealed class SiteRenderOutputDto
    {
        [JsonPropertyName("SiteTitle")] public string SiteTitle { get; set; } = "";
        [JsonPropertyName("PageTitle")] public string PageTitle { get; set; } = "";
        [JsonPropertyName("Nav")] public List<NavLinkDto>? Nav { get; set; }
        [JsonPropertyName("Breadcrumbs")] public List<CrumbDto>? Breadcrumbs { get; set; }
        [JsonPropertyName("Sidebar")] public List<SectionLinkDto>? Sidebar { get; set; }
        [JsonPropertyName("BodyMarkdown")] public string BodyMarkdown { get; set; } = "";
        [JsonPropertyName("BodyFormat")] public string BodyFormat { get; set; } = "";
        [JsonPropertyName("PeerID")] public string PeerID { get; set; } = "";
        [JsonPropertyName("SiteID")] public string SiteID { get; set; } = "";
        [JsonPropertyName("CurrentPage")] public string CurrentPage { get; set; } = "";
        [JsonPropertyName("Error")] public string Error { get; set; } = "";
        [JsonPropertyName("Loading")] public bool Loading { get; set; }
        [JsonPropertyName("CanGoBack")] public bool CanGoBack { get; set; }
    }

    private sealed class NavLinkDto
    {
        [JsonPropertyName("Label")] public string Label { get; set; } = "";
        [JsonPropertyName("Target")] public string Target { get; set; } = "";
        [JsonPropertyName("Active")] public bool Active { get; set; }
        [JsonPropertyName("Kind")] public string Kind { get; set; } = "";
    }

    private sealed class CrumbDto
    {
        [JsonPropertyName("Label")] public string Label { get; set; } = "";
        [JsonPropertyName("Target")] public string Target { get; set; } = "";
    }

    private sealed class SectionLinkDto
    {
        [JsonPropertyName("Label")] public string Label { get; set; } = "";
        [JsonPropertyName("Target")] public string Target { get; set; } = "";
        [JsonPropertyName("Active")] public bool Active { get; set; }
        [JsonPropertyName("Depth")] public int Depth { get; set; }
        [JsonPropertyName("IsSection")] public bool IsSection { get; set; }
    }
}
