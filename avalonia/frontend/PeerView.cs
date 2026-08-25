using System;
using System.Text.Json;
using System.Text.Json.Serialization;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Layout;
using Avalonia.Media;
using EntityAvalonia.Panels;

namespace EntityAvalonia;

// PeerView is the per-peer UserControl — one instance per tab. Owns
// everything that lives per-peer except the dispatch surface, which
// since I.5 wave 2 is owned by ShellPanel (one shell per panel
// instance via shellcmd.NewShellInWorkspace).
//
// PHASE-I-MULTI-PEER-PLAN.md §5.3 — extracted from MainWindow's
// single-peer flat layout. Each PeerView constructs its own
// PeerResolver with the peer's handle as the active override; panels
// inside this view bind to that override via the resolver (not by
// receiving the handle as a free-floating argument).
//
// PHASE-I-DESKTOP-RENDERER-PLAN §I.5 wave 2 — the right column is
// now just the PanelStack. Default boot includes a shell panel so
// the dispatch surface is still present at startup; the user can
// close it, open more, or rearrange via the stack chrome.
//
// IPanelHost surface — broadcasts tree selection to detail-shaped
// panels, and accepts peer-status refresh requests from shells.
//
// Lifetime: created when a peer's tab opens; disposed when the tab
// closes. Disposing tears down the tree + panel stack. PeerDestroy
// on the underlying handle is the caller's responsibility (MainWindow
// owns peer lifecycle).
public sealed class PeerView : UserControl, IDisposable, IPanelHost
{
    public long PeerHandle { get; }
    public string Alias { get; private set; } = "";
    public PeerResolver Resolver { get; }

    // IPanelHost — broadcast tree selection to any DetailPanel-shaped
    // panel currently mounted in a switchable slot.
    public event Action<string>? SelectedPath;
    public string? CurrentSelectedPath { get; private set; }

    private readonly TreeViewPanel _tree;
    private readonly PanelStack _panelStack;

    // Per-peer status bar — shows alias, peer-id, identity, connections.
    private readonly SelectableTextBlock _peerStatus;

    private bool _disposed;

    public PeerView(long peerHandle, long systemPeerHandle)
    {
        PeerHandle = peerHandle;
        Resolver = new PeerResolver(systemPeerHandle, activePeerOverride: peerHandle);

        _peerStatus = new SelectableTextBlock
        {
            Text = "(loading peer status…)",
            Opacity = 0.6,
            Margin = new Thickness(12, 8),
            FontSize = 13,
        };

        // Tree stays hard-mounted as the left-column navigator — it's
        // structurally privileged. Tree selection feeds IPanelHost.
        // SelectedPath so any DetailPanel-shaped slot can subscribe
        // without holding a direct reference to the tree.
        _tree = new TreeViewPanel(Resolver.ResolveForPanel(PanelScope.Peer));
        _tree.EntitySelected += OnTreeEntitySelected;

        // Right column is a dynamic PanelStack. Default boot: site-view
        // (primary content), detail (selection sink), shell (dispatch
        // surface). User can close any of them via the slot's ✕ button
        // and add more via the stack's "+ Add panel".
        var peerH = Resolver.ResolveForPanel(PanelScope.Peer);
        _panelStack = new PanelStack(peerH, this, "site-view", "detail", "shell");

        // --- Left+right horizontal split.
        //
        // MinWidth/MaxWidth pin both columns — same structural
        // mitigation applied in PanelStack (see its class doc). The
        // crashes included a splitter drag here too; an
        // unbounded star column can be driven to zero by the splitter,
        // triggering the same Avalonia layout-engine self-recursion
        // that crashes the CLR.
        var split = new Grid();
        split.ColumnDefinitions.Add(new ColumnDefinition(new GridLength(360))
        {
            MinWidth = 200,
            MaxWidth = 700,
        });
        split.ColumnDefinitions.Add(new ColumnDefinition(new GridLength(4)));
        split.ColumnDefinitions.Add(new ColumnDefinition(GridLength.Star)
        {
            MinWidth = 400,
        });
        var treeContainer = new Border
        {
            Child = _tree,
            BorderBrush = new SolidColorBrush(Color.FromArgb(0x33, 0xff, 0xff, 0xff)),
            BorderThickness = new Thickness(0, 0, 1, 0),
        };
        var splitter = new GridSplitter
        {
            Background = new SolidColorBrush(Color.FromArgb(0x33, 0xff, 0xff, 0xff)),
            ResizeDirection = GridResizeDirection.Columns,
        };
        Grid.SetColumn(treeContainer, 0);
        Grid.SetColumn(splitter, 1);
        Grid.SetColumn(_panelStack, 2);
        split.Children.Add(treeContainer);
        split.Children.Add(splitter);
        split.Children.Add(_panelStack);

        var root = new DockPanel { LastChildFill = true };
        DockPanel.SetDock(_peerStatus, Dock.Top);
        root.Children.Add(_peerStatus);
        root.Children.Add(split);
        Content = root;

        RefreshPeerStatus();
    }

    // FocusInput delegates to the first shell panel in the stack, if
    // any. Tab-open puts the user at a shell prompt by default; with
    // per-panel shells we focus the topmost one.
    public void FocusInput()
    {
        for (int i = 0; i < _panelStack.SlotCountForTests; i++)
        {
            if (_panelStack.SlotAtForTests(i).CurrentPanelControlForSmoke
                is ShellPanel sp)
            {
                sp.FocusInput();
                return;
            }
        }
    }

    // Smoke-driver hooks. Used by SmokeDriver to drive the panel
    // pipeline programmatically under Xvfb (see SmokeDriver.cs +
    // PHASE-I-RELIABILITY-PLAN.md). Not for general use.
    internal TreeViewPanel TreeForSmoke => _tree;
    internal PanelStack PanelStackForSmoke => _panelStack;

    // SwitchMiddleSlotForSmoke / SwitchBottomSlotForSmoke retained for
    // the existing markdown-cycle driver — both delegate to the first
    // and last slots of the stack respectively. If the user has closed
    // a slot, the driver finds the nearest live slot (idx 0 or -1).
    internal void SwitchMiddleSlotForSmoke(string panelName)
    {
        if (_panelStack.SlotCountForTests > 0)
            _panelStack.SlotAtForTests(0).SwitchTo(panelName);
    }
    internal void SwitchBottomSlotForSmoke(string panelName)
    {
        var n = _panelStack.SlotCountForTests;
        if (n > 0)
            _panelStack.SlotAtForTests(n - 1).SwitchTo(panelName);
    }

    // SITE smoke surface — yields the live SiteViewPanel from whichever
    // slot is hosting one, or null if no slot holds a site-view.
    internal Panels.SiteViewPanel? SiteForSmoke
    {
        get
        {
            for (int i = 0; i < _panelStack.SlotCountForTests; i++)
            {
                if (_panelStack.SlotAtForTests(i).CurrentPanelControlForSmoke
                    is Panels.SiteViewPanel sp)
                {
                    return sp;
                }
            }
            return null;
        }
    }

    // ASTEROIDS smoke surface — same shape as SnakeForSmoke, for the
    // heterogeneous-actor compute-program panel driver (WB_SMOKE_ASTEROIDS).
    internal Panels.AsteroidsGamePanel? AsteroidsForSmoke
    {
        get
        {
            for (int i = 0; i < _panelStack.SlotCountForTests; i++)
            {
                if (_panelStack.SlotAtForTests(i).CurrentPanelControlForSmoke
                    is Panels.AsteroidsGamePanel ap)
                {
                    return ap;
                }
            }
            return null;
        }
    }

    // SNAKE smoke surface — same shape as SiteForSmoke, for the
    // compute-program panel driver (WB_SMOKE_SNAKE).
    internal Panels.SnakeGamePanel? SnakeForSmoke
    {
        get
        {
            for (int i = 0; i < _panelStack.SlotCountForTests; i++)
            {
                if (_panelStack.SlotAtForTests(i).CurrentPanelControlForSmoke
                    is Panels.SnakeGamePanel sp)
                {
                    return sp;
                }
            }
            return null;
        }
    }

    // LIFE smoke surface — same shape as SnakeForSmoke, for the
    // display-only compute-program panel driver (WB_SMOKE_LIFE).
    internal Panels.LifeGamePanel? LifeForSmoke
    {
        get
        {
            for (int i = 0; i < _panelStack.SlotCountForTests; i++)
            {
                if (_panelStack.SlotAtForTests(i).CurrentPanelControlForSmoke
                    is Panels.LifeGamePanel lp)
                {
                    return lp;
                }
            }
            return null;
        }
    }

    // HANDLER-BROWSER smoke surface (WB_SMOKE_HANDLERS). Same shape as
    // the others: find the mounted panel so the driver can select and
    // execute against it under real X11.
    internal Panels.HandlerBrowserPanel? HandlersForSmoke
    {
        get
        {
            for (int i = 0; i < _panelStack.SlotCountForTests; i++)
            {
                if (_panelStack.SlotAtForTests(i).CurrentPanelControlForSmoke
                    is Panels.HandlerBrowserPanel hp)
                {
                    return hp;
                }
            }
            return null;
        }
    }

    // PEER-CONNECTIONS smoke surface (WB_SMOKE_CONNECTIONS). Reaches the
    // panel that renders BOTH connection surfaces — the local pool and
    // the tree's liveness record — under real X11.
    internal Panels.PeerConnectionsPanel? ConnectionsForSmoke
    {
        get
        {
            for (int i = 0; i < _panelStack.SlotCountForTests; i++)
            {
                if (_panelStack.SlotAtForTests(i).CurrentPanelControlForSmoke
                    is Panels.PeerConnectionsPanel cp)
                {
                    return cp;
                }
            }
            return null;
        }
    }

    // GENERIC-HOST smoke surface — the one accessor for ALL programs mounted
    // through ProgramPanel (WB_SMOKE_PROGRAM=life|snake|asteroids).
    //
    // Note there is exactly one of these, where the three above are one per
    // program. That collapse is the rung, visible in the test surface.
    internal Panels.ProgramPanel? ProgramForSmoke
    {
        get
        {
            for (int i = 0; i < _panelStack.SlotCountForTests; i++)
            {
                if (_panelStack.SlotAtForTests(i).CurrentPanelControlForSmoke
                    is Panels.ProgramPanel pp)
                {
                    return pp;
                }
            }
            return null;
        }
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        _tree.Dispose();
        _panelStack.Dispose();
        // The underlying peer handle is NOT destroyed here — MainWindow
        // owns peer lifecycle and calls Bridge.PeerDestroy on tab close.
    }

    private void OnTreeEntitySelected(string path) => PublishSelectedPath(path);

    public void PublishSelectedPath(string path)
    {
        if (string.IsNullOrEmpty(path)) return;
        CurrentSelectedPath = path;
        SelectedPath?.Invoke(path);
    }

    // IPanelHost — ShellPanel calls this after every dispatch because
    // connect/disconnect/cd/identity commands mutate peer state shared
    // across all shells.
    public void RequestPeerStatusRefresh() => RefreshPeerStatus();

    private void RefreshPeerStatus()
    {
        var reply = Bridge.TakeString(Bridge.PeerSummary(PeerHandle));
        PeerSummaryDto? dto = null;
        try
        {
            dto = JsonSerializer.Deserialize<PeerSummaryDto>(reply);
        }
        catch { }
        if (dto == null || !dto.Ok)
        {
            _peerStatus.Text = $"peer status unavailable: {reply}";
            _peerStatus.Foreground = Brushes.IndianRed;
            return;
        }
        Alias = dto.Alias;
        var identity = string.IsNullOrEmpty(dto.Identity) ? "ephemeral" : $"identity={dto.Identity}";
        var peerShort = dto.PeerId.Length > 12 ? dto.PeerId.Substring(0, 12) + "…" : dto.PeerId;
        var sys = (PeerHandle == Resolver.SystemPeerHandle) ? " · SYSTEM" : "";
        _peerStatus.Text = $"@{dto.Alias} · {peerShort} · {identity} · {dto.Connections} remote{sys}";
        _peerStatus.Opacity = 0.85;
        _peerStatus.ClearValue(TextBlock.ForegroundProperty);
    }

    // --- DTOs --------------------------------------------------------

    private sealed class PeerSummaryDto
    {
        [JsonPropertyName("ok")] public bool Ok { get; set; }
        [JsonPropertyName("alias")] public string Alias { get; set; } = "";
        [JsonPropertyName("peer_id")] public string PeerId { get; set; } = "";
        [JsonPropertyName("identity")] public string Identity { get; set; } = "";
        [JsonPropertyName("connections")] public int Connections { get; set; }
    }
}
