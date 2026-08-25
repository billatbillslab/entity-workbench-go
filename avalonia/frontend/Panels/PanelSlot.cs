using System;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Controls.Primitives;
using Avalonia.Controls.Templates;
using Avalonia.Layout;
using Avalonia.Media;

namespace EntityAvalonia.Panels;

// PanelSlot is the swappable slot host. Each slot has:
//   - A header bar with the current panel's display name + a "▼"
//     button that opens a flyout menu listing every registered panel.
//   - A content area that holds the current panel control.
//
// Switching panels disposes the old one (if it's IDisposable) and
// constructs the new one via PanelRegistry. The owning PeerView keeps
// the slot's reference; the slot keeps the panel's reference. Lifetime
// flows top-down.
//
// Slots are NOT splittable today — each is one rectangle holding one
// panel. Splitting is a future workspace-level concern; v1 has a
// fixed grid in PeerView with three switchable slots.
public sealed class PanelSlot : UserControl, IDisposable
{
    private readonly long _peerHandle;
    private readonly IPanelHost _host;
    private readonly TextBlock _label;
    private readonly Button _picker;
    private readonly Button _closeBtn;
    private readonly Border _contentBorder;

    private string _currentName = "";
    private Control? _currentPanel;
    private bool _disposed;

    public string CurrentPanelName => _currentName;

    // RequestClose fires when the user clicks the slot's ✕ button.
    // The parent (PanelStack) handles the request by disposing this
    // slot and removing it from the layout. PanelSlot itself does NOT
    // dispose on its own click — close lifetime is the parent's call,
    // because the parent may want to refuse (e.g. last-slot guard).
    public event Action<PanelSlot>? RequestClose;

    // PanelChanged fires after SwitchTo swaps the mounted panel. PanelStack
    // listens so a slot's row can re-pin its MinHeight to the new panel's
    // declared need (IPanelPreferredHeight) — a text panel and a game board do
    // not want the same amount of vertical space.
    public event Action<PanelSlot>? PanelChanged;

    // PreferredSlotMinHeight is the mounted panel's declared minimum, or 0 when
    // it declares none (the overwhelming majority — they take the stack default).
    internal double PreferredSlotMinHeight =>
        (_currentPanel as IPanelPreferredHeight)?.PreferredSlotMinHeight ?? 0;

    // Smoke-test surface: the live panel control hosted by this slot.
    // Used by SmokeDriver to drive a panel without going through the
    // user-facing slot picker. Null if the slot is empty.
    internal Control? CurrentPanelControlForSmoke => _currentPanel;

    // Test surface: simulate a click on the slot's close button. Useful
    // for headless tests that want to exercise the close path without
    // routing through a Window + pointer-event simulation.
    internal void CloseForTests() => RequestClose?.Invoke(this);

    public PanelSlot(long peerHandle, IPanelHost host, string initialPanel)
    {
        _peerHandle = peerHandle;
        _host = host;

        _label = new TextBlock
        {
            FontSize = 12,
            FontWeight = FontWeight.SemiBold,
            Opacity = 0.75,
            VerticalAlignment = VerticalAlignment.Center,
        };

        _picker = new Button
        {
            Content = "▼",
            FontSize = 10,
            Padding = new Thickness(6, 2),
            Margin = new Thickness(6, 0, 0, 0),
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
        };
        ToolTip.SetTip(_picker, "Switch panel");
        _picker.Click += OnPickerClick;

        _closeBtn = new Button
        {
            Content = "✕",
            FontSize = 11,
            Padding = new Thickness(6, 2),
            Margin = new Thickness(2, 0, 4, 0),
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
        };
        ToolTip.SetTip(_closeBtn, "Close panel");
        _closeBtn.Click += OnCloseClick;

        // Right side: picker + close, in that order so close sits on
        // the far right (the conventional spot for window close glyphs).
        var rightChrome = new StackPanel
        {
            Orientation = Orientation.Horizontal,
        };
        rightChrome.Children.Add(_picker);
        rightChrome.Children.Add(_closeBtn);

        var header = new DockPanel
        {
            LastChildFill = true,
            Background = new SolidColorBrush(Color.FromArgb(0x14, 0xff, 0xff, 0xff)),
            Margin = new Thickness(0),
        };
        DockPanel.SetDock(rightChrome, Dock.Right);
        header.Children.Add(rightChrome);
        var labelHost = new Border
        {
            Child = _label,
            Padding = new Thickness(10, 4, 6, 4),
        };
        header.Children.Add(labelHost);

        _contentBorder = new Border
        {
            BorderThickness = new Thickness(0),
        };

        var dock = new DockPanel { LastChildFill = true };
        DockPanel.SetDock(header, Dock.Top);
        dock.Children.Add(header);
        dock.Children.Add(_contentBorder);
        Content = dock;

        SwitchTo(initialPanel);
    }

    public void SwitchTo(string panelName)
    {
        if (_currentName == panelName) return;

        if (_currentPanel is IDisposable d)
        {
            d.Dispose();
        }
        _currentPanel = null;

        var entry = PanelRegistry.Get(panelName);
        var displayName = entry?.DisplayName ?? panelName;
        _currentName = panelName;
        _label.Text = displayName;

        _currentPanel = PanelRegistry.Create(panelName, _peerHandle, _host);
        _contentBorder.Child = _currentPanel;
        PanelChanged?.Invoke(this);
    }

    private void OnCloseClick(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        RequestClose?.Invoke(this);
    }

    private void OnPickerClick(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        // MenuFlyout is the showAt-anchored variant. ContextMenu.Open
        // without an associated control throws ArgumentNullException;
        // MenuFlyout.ShowAt(anchor) doesn't.
        var flyout = new MenuFlyout
        {
            Placement = PlacementMode.BottomEdgeAlignedRight,
        };
        foreach (var entry in PanelRegistry.All())
        {
            var item = new MenuItem
            {
                Header = entry.DisplayName,
                FontSize = 13,
            };
            var name = entry.Name;
            item.Click += (_, _) => SwitchTo(name);
            flyout.Items.Add(item);
        }
        flyout.ShowAt(_picker);
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        if (_currentPanel is IDisposable d)
        {
            d.Dispose();
        }
        _currentPanel = null;
    }
}
