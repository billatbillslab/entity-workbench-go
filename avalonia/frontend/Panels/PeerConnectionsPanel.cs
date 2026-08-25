using System;
using System.Collections.Generic;
using System.Collections.ObjectModel;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Threading.Tasks;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Controls.Templates;
using Avalonia.Input;
using Avalonia.Layout;
using Avalonia.Media;
using Avalonia.Threading;

namespace EntityAvalonia.Panels;

// PeerConnectionsPanel renders the peer's alias → connection bindings
// and lets the user dial new remote peers. Driven by the bridge's
// connections handle (peer_connections.go).
//
// PHASE-I-PEER-CONNECTIONS-PLAN B-1. Today accepts only TCP addresses
// (bare host:port or tcp://host:port); ws:// is rejected with a clear
// error until core-go ships WebSocket transport (B-3).
//
// Wake source: the bridge subscribes to `system/peer/transport/` on
// the peer's store, so any Connect/Disconnect — from this panel or a
// ShellPanel on the same peer — triggers a refresh.
//
// TWO DIFFERENT ANSWERS, BOTH SHOWN, NEITHER MISLABELLED. The
// "Connections" list at the bottom is the local connection POOL: the
// aliases this peer can dial or drop, which is what the connect /
// disconnect buttons act on. The "Liveness" section is the TREE's
// lifecycle record (`system/peer/status`, EXTENSION-NETWORK §3.13) —
// the same view a remote consumer or the reconnect graph sees. They
// are not the same set and are not expected to agree: a peer that
// dialed US appears only in liveness, a connection evicted without a
// demotion leaves liveness saying `connected`, and only liveness can
// say `suspect` or say why a peer went away. Showing one under the
// other's name is the mistake the split exists to prevent.
public sealed class PeerConnectionsPanel : UserControl, IDisposable
{
    private readonly long _peerHandle;
    private readonly long _handle;
    private readonly long _discoveryHandle;
    private readonly long _livenessHandle;
    private readonly TextBox _addressInput;
    private readonly TextBox _aliasInput;
    private readonly Button _connectButton;
    private readonly TextBlock _statusLine;
    private readonly TextBlock _header;
    private readonly TextBlock _listenLine;
    private readonly TextBlock _nearbyHeader;
    private readonly TextBlock _nearbyPlaceholder;
    private readonly ListBox _connList;
    private readonly ListBox _nearbyList;
    private readonly TextBlock _livenessHeader;
    private readonly TextBlock _livenessPlaceholder;
    private readonly TextBlock _connListCaption;
    private readonly ListBox _livenessList;
    private readonly ObservableCollection<ConnVm> _connections = new();
    private readonly ObservableCollection<NearbyVm> _nearby = new();
    private readonly ObservableCollection<LiveVm> _liveness = new();

    private Bridge.TreeWakeCallback? _wakeCallback;
    private Bridge.TreeWakeCallback? _discoveryWakeCallback;
    private Bridge.TreeWakeCallback? _livenessWakeCallback;
    // Explicit GC roots — see TreeViewPanel for full rationale.
    private GCHandle _wakeCallbackHandle;
    private GCHandle _discoveryWakeCallbackHandle;
    private GCHandle _livenessWakeCallbackHandle;
    private bool _renderQueued;
    private bool _nearbyRenderQueued;
    private bool _livenessRenderQueued;
    private bool _disposed;

    // Test-only accessors (InternalsVisibleTo Workbench.Headless.Tests).
    internal long HandleForTests => _handle;
    internal long DiscoveryHandleForTests => _discoveryHandle;
    internal long LivenessHandleForTests => _livenessHandle;
    internal int LivenessCountForTests => _liveness.Count;
    internal string LivenessHeaderForTests => _livenessHeader.Text ?? "";
    internal string LivenessPlaceholderForTests => _livenessPlaceholder.Text ?? "";
    internal bool LivenessPlaceholderVisibleForTests => _livenessPlaceholder.IsVisible;
    internal string LivenessStatusAtForTests(int i) => _liveness[i].Status;
    internal string LivenessPeerIdAtForTests(int i) => _liveness[i].PeerID;
    internal void RerenderLivenessForTests() => RerenderLivenessFromBridge();
    internal int ConnectionCountForTests => _connections.Count;
    internal int NearbyCountForTests => _nearby.Count;
    internal string NearbyPeerIdAtForTests(int i) => _nearby[i].PeerID;
    internal bool NearbyConnectedAtForTests(int i) => _nearby[i].Connected;
    internal void TriggerNearbyConnectForTests(int i) => DoConnectFromNearby(_nearby[i]);
    // Sync test entry (kept for back-compat with the headless mount
    // test); fires-and-forgets the async version. Tests that need to
    // observe the resulting collection should use
    // RerenderNearbyAsyncForTests + await.
    internal void RerenderNearbyForTests() => _ = RerenderNearbyFromBridgeAsync();
    internal string AliasAtForTests(int i) => _connections[i].Alias;
    internal string AddressAtForTests(int i) => _connections[i].Address;
    internal bool IsLocalAtForTests(int i) => _connections[i].IsLocal;
    internal string StatusTextForTests => _statusLine.Text ?? "";
    internal string ListenLineForTests => _listenLine.Text ?? "";
    internal void SetAddressForTests(string s) => _addressInput.Text = s;
    internal void SetAliasForTests(string s) => _aliasInput.Text = s;
    internal Task TriggerConnectForTests() => DoConnectAsync();
    internal void TriggerDisconnectForTests(string alias) => DoDisconnect(alias);
    internal void RerenderForTests() => RerenderFromBridge();
    internal Task RerenderNearbyAsyncForTests() => RerenderNearbyFromBridgeAsync();

    public PeerConnectionsPanel(long peerHandle)
    {
        _peerHandle = peerHandle;
        var openReply = Bridge.TakeString(Bridge.ConnectionsOpen(peerHandle));
        _handle = ParseHandle(openReply);
        if (_handle < 0)
        {
            Content = new SelectableTextBlock
            {
                Text = $"peer-connections open failed: {openReply}",
                Foreground = Brushes.IndianRed,
                Margin = new Thickness(12),
                FontSize = 14,
            };
            _addressInput = new TextBox();
            _aliasInput = new TextBox();
            _connectButton = new Button();
            _statusLine = new TextBlock();
            _header = new TextBlock();
            _listenLine = new TextBlock();
            _nearbyHeader = new TextBlock();
            _nearbyPlaceholder = new TextBlock();
            _connList = new ListBox();
            _nearbyList = new ListBox();
            _livenessHeader = new TextBlock();
            _livenessPlaceholder = new TextBlock();
            _connListCaption = new TextBlock();
            _livenessList = new ListBox();
            _discoveryHandle = -1;
            _livenessHandle = -1;
            return;
        }

        // Open discovery handle alongside connections. It's OK for this
        // to fail (peer constructed without ListenAddr) — we just hide
        // the "Nearby peers" section in that case.
        var discoveryReply = Bridge.TakeString(Bridge.DiscoveryOpen(peerHandle));
        _discoveryHandle = ParseHandle(discoveryReply);

        // Liveness handle: the tree's lifecycle record. Unlike discovery
        // this needs no peer configuration — every peer has a status
        // namespace, empty until the first transition — so a failure
        // here is a real error, not a "feature off" case. We still
        // degrade to hiding the section rather than failing the panel.
        var livenessReply = Bridge.TakeString(Bridge.LivenessOpen(peerHandle));
        _livenessHandle = ParseHandle(livenessReply);

        _header = new TextBlock
        {
            Text = "Peer Connections",
            FontWeight = FontWeight.SemiBold,
            FontSize = 14,
            Margin = new Thickness(0, 0, 0, 6),
            Opacity = 0.8,
        };

        _listenLine = new TextBlock
        {
            Text = FormatListenLine(peerHandle),
            FontFamily = new FontFamily("monospace"),
            FontSize = 11,
            Opacity = 0.65,
            Margin = new Thickness(0, 0, 0, 6),
            TextWrapping = TextWrapping.Wrap,
        };

        _nearbyHeader = new TextBlock
        {
            Text = "Nearby peers",
            FontWeight = FontWeight.SemiBold,
            FontSize = 12,
            Opacity = 0.7,
            Margin = new Thickness(0, 4, 0, 4),
            IsVisible = _discoveryHandle >= 0,
        };

        // Placeholder shown when the list is empty. Replaces silent
        // emptiness with the actual state — "Searching…" before the
        // first scan returns, "No peers found" after. Hidden once any
        // peer appears.
        _nearbyPlaceholder = new TextBlock
        {
            Text = "Searching…",
            FontSize = 11,
            FontStyle = FontStyle.Italic,
            Opacity = 0.45,
            Margin = new Thickness(0, 0, 0, 4),
            IsVisible = _discoveryHandle >= 0,
        };

        _nearbyList = new ListBox
        {
            ItemsSource = _nearby,
            FontFamily = new FontFamily("monospace"),
            FontSize = 12,
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
            MaxHeight = 120,
            IsVisible = _discoveryHandle >= 0,
            ItemTemplate = new FuncDataTemplate<NearbyVm>((vm, _) =>
            {
                var grid = new Grid
                {
                    ColumnDefinitions = new ColumnDefinitions("*,Auto"),
                };
                var info = new StackPanel
                {
                    Orientation = Orientation.Vertical,
                    VerticalAlignment = VerticalAlignment.Center,
                };
                info.Children.Add(new SelectableTextBlock
                {
                    Text = vm.ShortPeerId,
                    FontSize = 12,
                });
                info.Children.Add(new SelectableTextBlock
                {
                    Text = string.IsNullOrEmpty(vm.DialURL) ? "(no address)" : vm.DialURL,
                    FontSize = 10,
                    Opacity = 0.55,
                });
                Grid.SetColumn(info, 0);

                Control trailing;
                if (vm.Connected)
                {
                    trailing = new TextBlock
                    {
                        Text = "Connected",
                        FontSize = 10,
                        Opacity = 0.55,
                        VerticalAlignment = VerticalAlignment.Center,
                    };
                }
                else
                {
                    var btn = new Button
                    {
                        Content = "Connect",
                        FontSize = 10,
                        Padding = new Thickness(6, 1),
                        VerticalAlignment = VerticalAlignment.Center,
                    };
                    btn.Click += (_, _) => DoConnectFromNearby(vm);
                    trailing = btn;
                }
                Grid.SetColumn(trailing, 1);

                grid.Children.Add(info);
                grid.Children.Add(trailing);
                return grid;
            }, supportsRecycling: true),
        };

        // --- Liveness section (the tree's answer) ------------------------
        _livenessHeader = new TextBlock
        {
            Text = "Liveness (tree)",
            FontWeight = FontWeight.SemiBold,
            FontSize = 12,
            Opacity = 0.7,
            Margin = new Thickness(0, 6, 0, 4),
            IsVisible = _livenessHandle >= 0,
        };

        // "Nothing recorded" is NOT "nobody is connected" — the status
        // entity is written on transition, so absence means "no
        // transition has ever been recorded for anyone", which is the
        // normal state of a peer that has not yet talked to another
        // peer. The placeholder says that rather than implying an
        // outage.
        _livenessPlaceholder = new TextBlock
        {
            Text = "No lifecycle transitions recorded.",
            FontSize = 11,
            FontStyle = FontStyle.Italic,
            Opacity = 0.45,
            Margin = new Thickness(0, 0, 0, 4),
            IsVisible = _livenessHandle >= 0,
        };

        _livenessList = new ListBox
        {
            ItemsSource = _liveness,
            FontFamily = new FontFamily("monospace"),
            FontSize = 12,
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
            MaxHeight = 140,
            IsVisible = _livenessHandle >= 0,
            ItemTemplate = new FuncDataTemplate<LiveVm>((vm, _) =>
            {
                var row = new StackPanel { Orientation = Orientation.Horizontal };
                row.Children.Add(new TextBlock
                {
                    Text = "●",
                    FontSize = 13,
                    Foreground = StatusBrush(vm.Status),
                    VerticalAlignment = VerticalAlignment.Center,
                    Margin = new Thickness(0, 0, 6, 0),
                });
                var info = new StackPanel
                {
                    Orientation = Orientation.Vertical,
                    VerticalAlignment = VerticalAlignment.Center,
                };
                info.Children.Add(new SelectableTextBlock
                {
                    Text = $"{vm.ShortPeerId}  {vm.Status}",
                    FontSize = 12,
                });
                info.Children.Add(new SelectableTextBlock
                {
                    Text = vm.Detail,
                    FontSize = 10,
                    Opacity = 0.55,
                });
                row.Children.Add(info);
                return row;
            }, supportsRecycling: true),
        };

        _connListCaption = new TextBlock
        {
            Text = "Connections (local pool)",
            FontWeight = FontWeight.SemiBold,
            FontSize = 12,
            Opacity = 0.7,
            Margin = new Thickness(0, 6, 0, 4),
        };

        _addressInput = new TextBox
        {
            Watermark = "host:port or tcp://host:port",
            FontFamily = new FontFamily("monospace"),
            FontSize = 13,
            Margin = new Thickness(0, 0, 0, 4),
        };
        _addressInput.KeyDown += OnInputKey;

        _aliasInput = new TextBox
        {
            Watermark = "alias (e.g. remote)",
            FontFamily = new FontFamily("monospace"),
            FontSize = 13,
            Margin = new Thickness(0, 0, 0, 4),
        };
        _aliasInput.KeyDown += OnInputKey;

        _connectButton = new Button
        {
            Content = "Connect",
            FontSize = 12,
            Padding = new Thickness(10, 4),
            HorizontalAlignment = HorizontalAlignment.Right,
            Margin = new Thickness(0, 0, 0, 6),
        };
        _connectButton.Click += (_, _) => _ = DoConnectAsync();

        _statusLine = new TextBlock
        {
            Text = "",
            FontFamily = new FontFamily("monospace"),
            FontSize = 12,
            Opacity = 0.7,
            Margin = new Thickness(0, 0, 0, 6),
            TextWrapping = TextWrapping.Wrap,
        };

        _connList = new ListBox
        {
            ItemsSource = _connections,
            FontFamily = new FontFamily("monospace"),
            FontSize = 13,
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
            ItemTemplate = new FuncDataTemplate<ConnVm>((vm, _) =>
            {
                var grid = new Grid
                {
                    ColumnDefinitions = new ColumnDefinitions("Auto,*,Auto"),
                };
                var dot = new TextBlock
                {
                    Text = vm.IsLocal ? "◆" : "●",
                    FontSize = 13,
                    Foreground = vm.IsLocal ? Brushes.SteelBlue : Brushes.MediumSeaGreen,
                    VerticalAlignment = VerticalAlignment.Center,
                    Margin = new Thickness(0, 0, 6, 0),
                };
                Grid.SetColumn(dot, 0);

                var info = new StackPanel
                {
                    Orientation = Orientation.Vertical,
                    VerticalAlignment = VerticalAlignment.Center,
                };
                info.Children.Add(new SelectableTextBlock
                {
                    Text = $"{vm.Alias}  {vm.ShortPeerId}",
                    FontSize = 13,
                });
                info.Children.Add(new SelectableTextBlock
                {
                    Text = vm.IsLocal ? "(local)" : (string.IsNullOrEmpty(vm.Address) ? "(no address)" : vm.Address),
                    FontSize = 11,
                    Opacity = 0.6,
                });
                Grid.SetColumn(info, 1);

                Control trailing;
                if (vm.IsLocal)
                {
                    trailing = new TextBlock
                    {
                        Text = "",
                        VerticalAlignment = VerticalAlignment.Center,
                    };
                }
                else
                {
                    var btn = new Button
                    {
                        Content = "Disconnect",
                        FontSize = 11,
                        Padding = new Thickness(8, 2),
                        VerticalAlignment = VerticalAlignment.Center,
                    };
                    btn.Click += (_, _) => DoDisconnect(vm.Alias);
                    trailing = btn;
                }
                Grid.SetColumn(trailing, 2);

                grid.Children.Add(dot);
                grid.Children.Add(info);
                grid.Children.Add(trailing);
                return grid;
            }, supportsRecycling: true),
        };

        var inputs = new StackPanel { Orientation = Orientation.Vertical };
        inputs.Children.Add(_addressInput);
        inputs.Children.Add(_aliasInput);
        inputs.Children.Add(_connectButton);
        inputs.Children.Add(_statusLine);

        var top = new StackPanel { Orientation = Orientation.Vertical };
        top.Children.Add(_header);
        top.Children.Add(_listenLine);
        top.Children.Add(_livenessHeader);
        top.Children.Add(_livenessPlaceholder);
        top.Children.Add(_livenessList);
        top.Children.Add(_nearbyHeader);
        top.Children.Add(_nearbyPlaceholder);
        top.Children.Add(_nearbyList);
        top.Children.Add(inputs);
        top.Children.Add(_connListCaption);

        var dock = new DockPanel
        {
            LastChildFill = true,
            Margin = new Thickness(10),
        };
        DockPanel.SetDock(top, Dock.Top);
        dock.Children.Add(top);
        dock.Children.Add(new ScrollViewer { Content = _connList });
        Content = dock;

        _wakeCallback = OnWakeFromGo;
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        var cbPtr = Marshal.GetFunctionPointerForDelegate(_wakeCallback);
        Bridge.TakeString(Bridge.ConnectionsRegisterWake(_handle, cbPtr));

        if (_discoveryHandle >= 0)
        {
            _discoveryWakeCallback = OnDiscoveryWakeFromGo;
            _discoveryWakeCallbackHandle = GCHandle.Alloc(_discoveryWakeCallback);
            var dPtr = Marshal.GetFunctionPointerForDelegate(_discoveryWakeCallback);
            Bridge.TakeString(Bridge.DiscoveryRegisterWake(_discoveryHandle, dPtr));
            _ = SettleNearbyPlaceholderAsync();
        }

        if (_livenessHandle >= 0)
        {
            _livenessWakeCallback = OnLivenessWakeFromGo;
            _livenessWakeCallbackHandle = GCHandle.Alloc(_livenessWakeCallback);
            var lPtr = Marshal.GetFunctionPointerForDelegate(_livenessWakeCallback);
            Bridge.TakeString(Bridge.LivenessRegisterWake(_livenessHandle, lPtr));
        }
        PanelLog.Write("peer-connections",
            $"Mount h={_handle} discovery={_discoveryHandle} liveness={_livenessHandle}");
    }

    // StatusBrush maps the three lifecycle states (EXTENSION-NETWORK
    // §3.13) to colour. `suspect` gets its own colour precisely because
    // it is the state the connection pool cannot represent — collapsing
    // it into either green or grey throws away the whole reason this
    // section exists.
    private static IBrush StatusBrush(string status) => status switch
    {
        "connected" => Brushes.MediumSeaGreen,
        "suspect" => Brushes.Goldenrod,
        "disconnected" => Brushes.Gray,
        _ => Brushes.DimGray,
    };

    private void OnLivenessWakeFromGo(long handle)
    {
        if (_disposed) return;
        if (_livenessRenderQueued) return;
        _livenessRenderQueued = true;
        Dispatcher.UIThread.Post(() =>
        {
            _livenessRenderQueued = false;
            if (_disposed) return;
            RerenderLivenessFromBridge();
        });
    }

    // RerenderLivenessFromBridge reads the model snapshot the bridge
    // holds open. Synchronous on purpose: the call is a mutex-guarded
    // copy of an already-materialised slice (no store read, no network
    // — see PeerLivenessModel.Render), which is a different cost class
    // from the discovery path that had to move off the UI thread.
    private void RerenderLivenessFromBridge()
    {
        if (_livenessHandle < 0 || _disposed) return;
        PanelLog.Write("peer-connections", $"LivenessRender h={_livenessHandle}");
        var reply = Bridge.TakeString(Bridge.LivenessRender(_livenessHandle));
        LivenessRenderDto? dto = null;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                _livenessHeader.Text = "Liveness (tree) — unavailable";
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<LivenessRenderDto>();
        }
        catch (Exception ex)
        {
            _livenessHeader.Text = $"Liveness (tree) — parse failed: {ex.Message}";
            return;
        }

        _liveness.Clear();
        if (dto?.Peers != null)
        {
            foreach (var p in dto.Peers)
            {
                _liveness.Add(new LiveVm(
                    p.PeerId, p.Status, p.Reason, p.LastError, p.ConnectedAt, p.FailingSince));
            }
        }
        _livenessPlaceholder.IsVisible = _liveness.Count == 0;

        // A failed seed and an empty namespace look identical in a list;
        // only one of them is fine, so the error goes in the header
        // instead of being swallowed into a blank section.
        if (!string.IsNullOrEmpty(dto?.SeedError))
        {
            _livenessHeader.Text = $"Liveness (tree) — read failed: {dto!.SeedError}";
            return;
        }
        _livenessHeader.Text = dto == null
            ? "Liveness (tree)"
            : $"Liveness (tree) — {dto.Connected} connected · {dto.Suspect} suspect · {dto.Disconnected} disconnected";
    }

    private void DoConnectFromNearby(NearbyVm vm)
    {
        if (_handle < 0 || _disposed) return;
        if (vm.Connected || string.IsNullOrEmpty(vm.DialURL)) return;
        // Pre-fill the address; auto-suggest an alias from the short
        // peer-id. User can edit before the dial fires.
        _addressInput.Text = vm.DialURL;
        if (string.IsNullOrEmpty(_aliasInput.Text))
        {
            _aliasInput.Text = vm.ShortPeerId.Replace("…", "").ToLowerInvariant();
        }
        _ = DoConnectAsync();
    }

    private void OnDiscoveryWakeFromGo(long handle)
    {
        if (_disposed) return;
        if (_nearbyRenderQueued) return;
        _nearbyRenderQueued = true;
        Dispatcher.UIThread.Post(() =>
        {
            _nearbyRenderQueued = false;
            if (_disposed) return;
            _ = RerenderNearbyFromBridgeAsync();
        });
    }

    private void RerenderNearbyFromBridge() => _ = RerenderNearbyFromBridgeAsync();

    // RerenderNearbyFromBridgeAsync dispatches the bridge call on a
    // thread-pool worker. The bridge call is cheap today (cgo + store
    // prefix read; no Scan), but staying off the UI thread is defense
    // in depth — same pattern DoConnectAsync uses for ConnectionsConnect.
    // Before this split, the bridge synchronously ran a 1.5s mDNS browse
    // on the UI thread on every wake; combined with the wake-on-store-
    // write feedback loop that produced a hard UI freeze in B-5 phase B.
    private async Task RerenderNearbyFromBridgeAsync()
    {
        if (_discoveryHandle < 0) return;
        PanelLog.Write("peer-connections", $"NearbyRender h={_discoveryHandle}");
        var handle = _discoveryHandle;
        string reply;
        try
        {
            reply = await Task.Run(() => Bridge.TakeString(Bridge.DiscoveryRender(handle)));
        }
        catch
        {
            return;
        }
        if (_disposed) return;
        NearbyRenderDto? dto = null;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                // Discovery substrate disabled or transient error — just
                // hide the section. Status line is reserved for connect
                // operations.
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<NearbyRenderDto>();
        }
        catch
        {
            return;
        }
        _nearby.Clear();
        if (dto?.Entries != null)
        {
            foreach (var e in dto.Entries)
            {
                _nearby.Add(new NearbyVm(e.PeerId, e.DialUrl, e.Backend, e.Connected));
            }
        }
        UpdateNearbyPlaceholder();
    }

    // UpdateNearbyPlaceholder hides the placeholder when the list has
    // entries; shows it otherwise. Whether the placeholder reads
    // "Searching…" vs "No peers found" is owned by the settle timer
    // kicked off at mount — the seed wake fires before the first
    // bridge-driven scan completes, so eagerly swapping to "No peers"
    // on first render lies.
    private void UpdateNearbyPlaceholder()
    {
        if (_discoveryHandle < 0) return;
        _nearbyPlaceholder.IsVisible = _nearby.Count == 0;
    }

    // SettleNearbyPlaceholderAsync swaps the placeholder text from
    // "Searching…" to "No peers found" after the bridge's first scan
    // has had time to complete (scanLoop cadence is 5s; +1s slack).
    // No-op if peers showed up first — UpdateNearbyPlaceholder hides
    // the TextBlock entirely in that case.
    private async Task SettleNearbyPlaceholderAsync()
    {
        await Task.Delay(6000);
        if (_disposed) return;
        if (_nearby.Count > 0) return;
        _nearbyPlaceholder.Text = "No peers found on the LAN.";
    }

    private void OnInputKey(object? sender, KeyEventArgs e)
    {
        if (e.Key == Key.Enter)
        {
            _ = DoConnectAsync();
            e.Handled = true;
        }
    }

    // DoConnectAsync dispatches the bridge connect call on a thread-pool
    // worker so the 10s context inside cmdConnect can't freeze the UI
    // pump on slow/unreachable dials. Inputs are disabled while the dial
    // is in flight to suppress button-mashing re-entrancy; the
    // SynchronizationContext default returns the continuation to the UI
    // thread for status + render updates.
    private async Task DoConnectAsync()
    {
        if (_handle < 0 || _disposed) return;
        var addr = (_addressInput.Text ?? "").Trim();
        var alias = (_aliasInput.Text ?? "").Trim();
        if (addr.Length == 0)
        {
            ShowError("address is required");
            return;
        }
        if (alias.Length == 0)
        {
            ShowError("alias is required");
            return;
        }
        PanelLog.Write("peer-connections", $"Connect h={_handle} alias='{alias}' addr='{addr}'");
        SetInputsEnabled(false);
        ShowInfo($"Connecting to {addr}...");
        try
        {
            var handle = _handle;
            var reply = await Task.Run(() =>
                Bridge.TakeString(Bridge.ConnectionsConnect(handle, alias, addr)));
            if (_disposed) return;
            if (!ParseOk(reply, out var err))
            {
                ShowError(err);
                return;
            }
            ShowInfo($"connected: {alias}");
            _addressInput.Text = "";
            _aliasInput.Text = "";
            // The wake from system/peer/transport/ will re-render shortly,
            // but call it directly so the list refreshes in the same tick.
            RerenderFromBridge();
        }
        finally
        {
            if (!_disposed) SetInputsEnabled(true);
        }
    }

    private void SetInputsEnabled(bool enabled)
    {
        _connectButton.IsEnabled = enabled;
        _addressInput.IsEnabled = enabled;
        _aliasInput.IsEnabled = enabled;
    }

    private void DoDisconnect(string alias)
    {
        if (_handle < 0 || _disposed) return;
        PanelLog.Write("peer-connections", $"Disconnect h={_handle} alias='{alias}'");
        var reply = Bridge.TakeString(Bridge.ConnectionsDisconnect(_handle, alias));
        if (!ParseOk(reply, out var err))
        {
            ShowError($"disconnect {alias}: {err}");
            return;
        }
        ShowInfo($"disconnected: {alias}");
        RerenderFromBridge();
    }

    private void ShowError(string msg)
    {
        _statusLine.Text = msg;
        _statusLine.Foreground = Brushes.IndianRed;
        _statusLine.Opacity = 1.0;
    }

    private void ShowInfo(string msg)
    {
        _statusLine.Text = msg;
        _statusLine.ClearValue(TextBlock.ForegroundProperty);
        _statusLine.Opacity = 0.7;
    }

    private void OnWakeFromGo(long handle)
    {
        if (_disposed) return;
        if (_renderQueued) return;
        _renderQueued = true;
        Dispatcher.UIThread.Post(() =>
        {
            _renderQueued = false;
            if (_disposed) return;
            RerenderFromBridge();
        });
    }

    private void RerenderFromBridge()
    {
        if (_handle < 0) return;
        PanelLog.Write("peer-connections", $"Render h={_handle}");
        var reply = Bridge.TakeString(Bridge.ConnectionsRender(_handle));
        RenderDto? dto = null;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                ShowError(reply);
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<RenderDto>();
        }
        catch (Exception ex)
        {
            ShowError($"render parse failed: {ex.Message}");
            return;
        }
        _connections.Clear();
        if (dto?.Entries != null)
        {
            foreach (var e in dto.Entries)
            {
                _connections.Add(new ConnVm(e.Alias, e.PeerId, e.Address, e.IsLocal));
            }
        }
        // Repaint nearby too — the workspace's connection set just changed,
        // so an entry's Connected flag may have flipped.
        if (_discoveryHandle >= 0)
        {
            RerenderNearbyFromBridge();
        }
    }

    // FormatListenLine queries the bridge for the peer's bound listen
    // address and returns a human label. Called once at mount — listen
    // address is set at peer-create time and stable across the peer's
    // lifetime (no need to re-render).
    private static string FormatListenLine(long peerHandle)
    {
        var reply = Bridge.TakeString(Bridge.PeerListenAddr(peerHandle));
        try
        {
            using var doc = JsonDocument.Parse(reply);
            var root = doc.RootElement;
            if (!root.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                return "Listen status: unavailable";
            }
            if (!root.TryGetProperty("result", out var result))
            {
                return "Listen status: unavailable";
            }
            if (!result.TryGetProperty("listening", out var listening) || !listening.GetBoolean())
            {
                return "Not listening (configure -listen to accept incoming peers)";
            }
            var scheme = result.TryGetProperty("scheme", out var s) ? (s.GetString() ?? "") : "";
            var addr = result.TryGetProperty("addr", out var a) ? (a.GetString() ?? "") : "";
            if (string.IsNullOrEmpty(addr))
            {
                return "Listening (address unknown)";
            }
            return string.IsNullOrEmpty(scheme)
                ? $"Listening at: {addr}"
                : $"Listening at: {scheme}://{addr}";
        }
        catch
        {
            return "Listen status: parse failed";
        }
    }

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

    private static bool ParseOk(string envelope, out string error)
    {
        error = "";
        try
        {
            using var doc = JsonDocument.Parse(envelope);
            var root = doc.RootElement;
            if (root.TryGetProperty("ok", out var ok) && ok.GetBoolean()) return true;
            if (root.TryGetProperty("error", out var err))
            {
                error = err.GetString() ?? "unknown error";
                return false;
            }
            error = envelope;
            return false;
        }
        catch (Exception ex)
        {
            error = $"parse failed: {ex.Message}";
            return false;
        }
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        PanelLog.Write("peer-connections", $"Dispose h={_handle}");
        if (_handle >= 0)
        {
            Bridge.ConnectionsClose(_handle);
        }
        if (_discoveryHandle >= 0)
        {
            Bridge.DiscoveryClose(_discoveryHandle);
        }
        if (_livenessHandle >= 0)
        {
            // LivenessClose joins its wake goroutine before returning,
            // so freeing the GCHandle below cannot race a callback.
            Bridge.LivenessClose(_livenessHandle);
        }
        _wakeCallback = null;
        _discoveryWakeCallback = null;
        _livenessWakeCallback = null;
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
        if (_discoveryWakeCallbackHandle.IsAllocated) _discoveryWakeCallbackHandle.Free();
        if (_livenessWakeCallbackHandle.IsAllocated) _livenessWakeCallbackHandle.Free();
    }

    private sealed class RenderDto
    {
        [JsonPropertyName("entries")] public List<EntryDto>? Entries { get; set; }
    }

    private sealed class EntryDto
    {
        [JsonPropertyName("alias")] public string Alias { get; set; } = "";
        [JsonPropertyName("peer_id")] public string PeerId { get; set; } = "";
        [JsonPropertyName("address")] public string Address { get; set; } = "";
        [JsonPropertyName("is_local")] public bool IsLocal { get; set; }
    }

    private sealed class ConnVm
    {
        public string Alias { get; }
        public string PeerId { get; }
        public string Address { get; }
        public bool IsLocal { get; }
        public string ShortPeerId =>
            string.IsNullOrEmpty(PeerId)
                ? ""
                : (PeerId.Length > 14 ? PeerId.Substring(0, 12) + "…" : PeerId);
        public ConnVm(string alias, string peerId, string address, bool isLocal)
        {
            Alias = alias;
            PeerId = peerId;
            Address = address;
            IsLocal = isLocal;
        }
    }

    private sealed class NearbyRenderDto
    {
        [JsonPropertyName("entries")] public List<NearbyEntryDto>? Entries { get; set; }
    }

    private sealed class NearbyEntryDto
    {
        [JsonPropertyName("peer_id")] public string PeerId { get; set; } = "";
        [JsonPropertyName("dial_url")] public string DialUrl { get; set; } = "";
        [JsonPropertyName("backend")] public string Backend { get; set; } = "";
        [JsonPropertyName("connected")] public bool Connected { get; set; }
    }

    private sealed class LivenessRenderDto
    {
        [JsonPropertyName("peers")] public List<LivenessEntryDto>? Peers { get; set; }
        [JsonPropertyName("connected")] public int Connected { get; set; }
        [JsonPropertyName("suspect")] public int Suspect { get; set; }
        [JsonPropertyName("disconnected")] public int Disconnected { get; set; }
        [JsonPropertyName("seed_error")] public string? SeedError { get; set; }
    }

    // No last_seen field, deliberately — the bridge does not emit one.
    // See Bridge.cs's liveness section and liveness.go's header: the
    // status entity is transition-written, so a "last seen" reading of
    // that stamp would be a freshness claim the protocol never made.
    private sealed class LivenessEntryDto
    {
        [JsonPropertyName("peer_id")] public string PeerId { get; set; } = "";
        [JsonPropertyName("status")] public string Status { get; set; } = "";
        [JsonPropertyName("reason")] public string Reason { get; set; } = "";
        [JsonPropertyName("last_error")] public string LastError { get; set; } = "";
        // ms since epoch, not a formatted string — the bridge passes the
        // model's uint64 straight through.
        [JsonPropertyName("connected_at")] public ulong ConnectedAt { get; set; }
        [JsonPropertyName("failing_since")] public ulong FailingSince { get; set; }
    }

    private sealed class LiveVm
    {
        public string PeerID { get; }
        public string Status { get; }
        public string Reason { get; }
        public string LastError { get; }
        public ulong ConnectedAt { get; }
        public ulong FailingSince { get; }
        public string ShortPeerId =>
            string.IsNullOrEmpty(PeerID)
                ? ""
                : (PeerID.Length > 14 ? PeerID.Substring(0, 12) + "…" : PeerID);

        // Detail is the "why", in the order a reader needs it: the
        // error if there is one, else the reason, else the stamp that
        // actually applies to the current state. `failing_since` and
        // `connected_at` mean what they look like (unlike last_seen).
        public string Detail
        {
            get
            {
                if (!string.IsNullOrEmpty(LastError)) return LastError;
                if (!string.IsNullOrEmpty(Reason)) return Reason;
                if (Status == "connected" && ConnectedAt != 0)
                    return $"since {Stamp(ConnectedAt)}";
                if (FailingSince != 0) return $"failing since {Stamp(FailingSince)}";
                return "";
            }
        }

        // Stamp renders ms-since-epoch as local wall-clock. Zero means
        // "not known" (the field is optional in §3.13), never epoch.
        private static string Stamp(ulong ms) =>
            DateTimeOffset.FromUnixTimeMilliseconds((long)ms).ToLocalTime().ToString("HH:mm:ss");

        public LiveVm(string peerID, string status, string reason,
            string lastError, ulong connectedAt, ulong failingSince)
        {
            PeerID = peerID;
            Status = status;
            Reason = reason;
            LastError = lastError;
            ConnectedAt = connectedAt;
            FailingSince = failingSince;
        }
    }

    private sealed class NearbyVm
    {
        public string PeerID { get; }
        public string DialURL { get; }
        public string Backend { get; }
        public bool Connected { get; }
        public string ShortPeerId =>
            string.IsNullOrEmpty(PeerID)
                ? ""
                : (PeerID.Length > 14 ? PeerID.Substring(0, 12) + "…" : PeerID);
        public NearbyVm(string peerID, string dialURL, string backend, bool connected)
        {
            PeerID = peerID;
            DialURL = dialURL;
            Backend = backend;
            Connected = connected;
        }
    }
}
