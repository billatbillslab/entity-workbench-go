using System;
using System.Collections.Generic;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Controls.Primitives;
using Avalonia.Layout;
using Avalonia.Media;

namespace EntityAvalonia.Panels;

// PanelStack is a vertical stack of N PanelSlots with resizable splits
// between them, hosted in a dynamic Grid wrapped in a ScrollViewer.
// The panel count is mutable:
//
//   - "+" button in the stack's header opens a flyout over
//     PanelRegistry.All() and appends a new slot at the bottom with
//     the chosen panel kind.
//   - Each child PanelSlot exposes a close (✕) button that fires
//     RequestClose; PanelStack handles it by disposing the slot and
//     rebuilding the grid.
//
// Layout invariants:
//
//   - Slots are stored in a List<PanelSlot> in display order
//     (top-to-bottom).
//   - Slot rows are star-weighted with RowDefinition.MinHeight =
//     SlotMinHeight and RowDefinition.MaxHeight = SlotMaxHeight pinned.
//     GridSplitter rows are fixed 4px. Empty stacks render a hint.
//   - Grid.Height is rebound on every SizeChanged to
//     max(viewport, sum_of_MinHeights). This produces the user-friendly
//     behavior: one panel fills the viewport; two panels split
//     50/50; N panels each share the viewport down to MinHeight, at
//     which point the stack overflows and the ScrollViewer scrolls.
//   - **Add appends without rebuilding** so existing splitter drags
//     are preserved when a new panel is added. Close still rebuilds —
//     loses drag state on the rare close path, which is acceptable.
//   - PanelStack does NOT clamp the slot count.
//
// Structural mitigation against upstream Avalonia layout-engine
// recursion. Two SIGSEGVs were captured (PIDs 4033208,
// 4035311) when a GridSplitter drag drove a star-weighted row to
// zero size: the layout pass self-recursed ~25 frames deep in
// libcoreclr.so and crashed the CLR. The fix is structural — pixel
// sizing + RowDefinition.MinHeight makes the zero-size condition
// unreachable so the recursion can never trigger. Same shape as the
// compositor-pin fix (open #4): when an upstream platform bug bites
// from below, clamp the inputs at the panel layer to make the
// triggering condition unreachable.
//
// I.5 multi-panel proper. The "bones" the earlier sessions named —
// PanelSlot for swap-in-place + PanelRegistry for the kind catalog —
// already existed; PanelStack is the layer that turns them into a
// truly mutable workspace.
public sealed class PanelStack : UserControl, IDisposable
{
    // Per-slot sizing. Pixel-default with hard min/max bounds — the
    // splitter respects RowDefinition.MinHeight/MaxHeight, which kills
    // the zero-collapse path that triggered Avalonia's layout recursion
    // bug. See class doc.
    internal const double SlotMinHeight = 200;
    internal const double SlotMaxHeight = 900;
    internal const double SplitterHeight = 4;

    // Right gutter reserved for the outer ScrollViewer's scrollbar.
    // Avalonia's fluent theme renders the scrollbar in overlay mode by
    // default — without an explicit gutter, the outer scrollbar overlays
    // any inner panel's scrollbar (LogViewer ListBox, MarkdownView
    // ScrollViewer, etc.) since they both sit at the same right edge.
    // 14px matches the fluent scrollbar's "expanded" width with a
    // little breathing room. Surfaced by host-run eyeball.
    internal const double ScrollGutter = 14;

    private readonly long _peerHandle;
    private readonly IPanelHost _host;
    private readonly List<PanelSlot> _slots = new();
    private readonly Grid _grid;
    private readonly ScrollViewer _scroll;
    private readonly Button _addBtn;
    private readonly TextBlock _stackLabel;
    private bool _disposed;

    // Test/smoke surface: read-only snapshot of currently-mounted
    // panel slot count + names. Used by SmokeDriver to verify the
    // dynamic layout invariants survive ingress traffic.
    internal int SlotCountForTests => _slots.Count;
    internal PanelSlot SlotAtForTests(int i) => _slots[i];
    internal ScrollViewer ScrollViewerForTests => _scroll;
    // RowDefinition for slot i. Splitter rows sit between slots:
    // slot 0 → row 0; slot i (i>0) → row 2*i. Used by tests to
    // pin MinHeight/MaxHeight invariants.
    internal RowDefinition SlotRowForTests(int i)
        => _grid.RowDefinitions[i == 0 ? 0 : 2 * i];

    public PanelStack(long peerHandle, IPanelHost host, params string[] initialPanels)
    {
        _peerHandle = peerHandle;
        _host = host;

        _stackLabel = new TextBlock
        {
            Text = "Panels",
            FontSize = 12,
            FontWeight = FontWeight.SemiBold,
            Opacity = 0.6,
            VerticalAlignment = VerticalAlignment.Center,
            Margin = new Thickness(10, 4, 0, 4),
        };

        _addBtn = new Button
        {
            Content = "+ Add panel",
            FontSize = 12,
            Padding = new Thickness(8, 2),
            Margin = new Thickness(0, 0, 4, 0),
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
        };
        ToolTip.SetTip(_addBtn, "Add a new panel below");
        _addBtn.Click += OnAddClick;

        var header = new DockPanel
        {
            LastChildFill = true,
            Background = new SolidColorBrush(Color.FromArgb(0x14, 0xff, 0xff, 0xff)),
        };
        DockPanel.SetDock(_addBtn, Dock.Right);
        header.Children.Add(_addBtn);
        header.Children.Add(_stackLabel);

        // Grid carries a right Margin = ScrollGutter so that inner panel
        // content (and their own scrollbars) physically sit ScrollGutter
        // pixels in from the right edge. The outer ScrollViewer's
        // overlay scrollbar then renders in that clean gutter with no
        // collision against inner scrollbars. See ScrollGutter doc.
        _grid = new Grid
        {
            Margin = new Thickness(0, 0, ScrollGutter, 0),
        };

        _scroll = new ScrollViewer
        {
            Content = _grid,
            HorizontalScrollBarVisibility = ScrollBarVisibility.Disabled,
            // Visible + AllowAutoHide=false keeps the scrollbar present
            // even when content fits the viewport, so the gutter never
            // visually "disappears" mid-resize. Combined with the Grid
            // Margin above, this means the user always sees a stable
            // right-edge region instead of a jumping scrollbar.
            VerticalScrollBarVisibility = ScrollBarVisibility.Visible,
            AllowAutoHide = false,
        };
        // Re-bind Grid.Height on every viewport size change so stars
        // resolve against max(viewport, content_min). See class doc.
        _scroll.SizeChanged += (_, _) => UpdateGridHeight();

        var root = new DockPanel { LastChildFill = true };
        DockPanel.SetDock(header, Dock.Top);
        root.Children.Add(header);
        root.Children.Add(_scroll);
        Content = root;

        foreach (var name in initialPanels)
        {
            AppendSlotInternal(name);
        }
        Rebuild();
        UpdateGridHeight();
    }

    public void AddSlot(string panelName)
    {
        if (_disposed) return;
        var wasEmpty = _slots.Count == 0;
        AppendSlotInternal(panelName);
        if (wasEmpty)
        {
            // Clear the empty-state hint built by Rebuild() previously.
            _grid.Children.Clear();
            _grid.RowDefinitions.Clear();
        }
        // Append (do NOT rebuild) so any existing GridSplitter drags
        // on the previous slot rows survive the add. Star resolution
        // re-divides the viewport share but absolute drags are preserved
        // (Avalonia GridSplitter converts to Star with adjusted weights).
        AppendRowsForSlot(_slots.Count - 1);
        UpdateGridHeight();
    }

    private void AppendSlotInternal(string panelName)
    {
        var slot = new PanelSlot(_peerHandle, _host, panelName);
        slot.RequestClose += OnSlotRequestClose;
        // The slot's ctor already ran SwitchTo, so the INITIAL pin comes from
        // AppendRowsForSlot; this covers every later swap-in-place.
        slot.PanelChanged += OnSlotPanelChanged;
        _slots.Add(slot);
    }

    private void OnSlotRequestClose(PanelSlot slot)
    {
        if (_disposed) return;
        if (!_slots.Remove(slot)) return;
        slot.RequestClose -= OnSlotRequestClose;
        slot.PanelChanged -= OnSlotPanelChanged;
        slot.Dispose();
        // Rebuild on close — simpler than splicing rows out, and
        // close-loses-drag-state is an acceptable trade-off for the
        // rare close case (vs add-loses-drag-state which the user
        // hits constantly).
        Rebuild();
        UpdateGridHeight();
    }

    // Rebuild repopulates the Grid from _slots from scratch. Called
    // on initial construction and on close. Add uses the lighter
    // AppendRowsForSlot path so existing drags survive.
    private void Rebuild()
    {
        _grid.Children.Clear();
        _grid.RowDefinitions.Clear();

        if (_slots.Count == 0)
        {
            _grid.RowDefinitions.Add(new RowDefinition(GridLength.Star));
            var hint = new TextBlock
            {
                Text = "(no panels — use \"+ Add panel\" above)",
                FontSize = 13,
                Opacity = 0.4,
                HorizontalAlignment = HorizontalAlignment.Center,
                VerticalAlignment = VerticalAlignment.Center,
                Margin = new Thickness(12),
            };
            Grid.SetRow(hint, 0);
            _grid.Children.Add(hint);
            return;
        }

        for (int i = 0; i < _slots.Count; i++)
        {
            AppendRowsForSlot(i);
        }
    }

    // AppendRowsForSlot appends the rows (preceding splitter when
    // i>0, then the slot's star row) for slot _slots[i] without
    // touching any rows that came before. Used by both Rebuild and
    // AddSlot — AddSlot relies on the "without touching" property so
    // existing GridSplitter-adjusted RowDefinitions survive an add.
    private void AppendRowsForSlot(int i)
    {
        if (i > 0)
        {
            _grid.RowDefinitions.Add(new RowDefinition(new GridLength(SplitterHeight)));
            var splitter = new GridSplitter
            {
                Background = new SolidColorBrush(Color.FromArgb(0x33, 0xff, 0xff, 0xff)),
                ResizeDirection = GridResizeDirection.Rows,
                HorizontalAlignment = HorizontalAlignment.Stretch,
            };
            Grid.SetRow(splitter, _grid.RowDefinitions.Count - 1);
            _grid.Children.Add(splitter);
        }
        // Star with MinHeight/MaxHeight pins. Splitter respects both,
        // and the MinHeight blocks the zero-collapse layout-recursion
        // SIGSEGV (see class doc).
        _grid.RowDefinitions.Add(new RowDefinition(GridLength.Star)
        {
            MinHeight = SlotMinHeightFor(_slots[i]),
            MaxHeight = SlotMaxHeight,
        });
        Grid.SetRow(_slots[i], _grid.RowDefinitions.Count - 1);
        _grid.Children.Add(_slots[i]);
    }

    // SlotMinHeightFor resolves a slot's row MinHeight: the stack default,
    // raised to whatever the mounted panel declares it needs
    // (IPanelPreferredHeight), clamped to SlotMaxHeight.
    //
    // The clamp is what keeps the class-doc invariant intact: the result is
    // always in [SlotMinHeight, SlotMaxHeight] and therefore always > 0, so a
    // panel cannot talk a row into the zero-size layout recursion.
    private static double SlotMinHeightFor(PanelSlot slot)
    {
        var want = Math.Max(SlotMinHeight, slot.PreferredSlotMinHeight);
        return Math.Min(want, SlotMaxHeight);
    }

    // OnSlotPanelChanged re-pins a slot's row when its panel is swapped in
    // place: switching a text panel to a game board (or back) changes what the
    // row needs, and without this the row keeps the old panel's floor.
    private void OnSlotPanelChanged(PanelSlot slot)
    {
        if (_disposed) return;
        var i = _slots.IndexOf(slot);
        if (i < 0) return;
        var row = _grid.RowDefinitions[i == 0 ? 0 : 2 * i];
        var want = SlotMinHeightFor(slot);
        if (Math.Abs(row.MinHeight - want) > 0.5)
        {
            row.MinHeight = want;
            UpdateGridHeight();
        }
    }

    // UpdateGridHeight rebinds Grid.Height to max(viewport, content_min).
    // - When viewport >= content_min: Grid.Height = viewport; star rows
    //   share viewport (1 panel fills, 2 split 50/50, etc.).
    // - When viewport < content_min: Grid.Height = content_min; star
    //   rows each clamp to MinHeight; the ScrollViewer engages.
    private void UpdateGridHeight()
    {
        if (_disposed) return;
        if (_slots.Count == 0)
        {
            // Empty state: let the hint row star naturally.
            if (!double.IsNaN(_grid.Height)) _grid.Height = double.NaN;
            return;
        }
        // Sum the slots' ACTUAL minimums, not count * SlotMinHeight — a slot
        // holding a panel that declared a larger floor contributes that floor,
        // and the stack has to grow (and scroll) to honour it.
        var contentMin = (_slots.Count - 1) * SplitterHeight;
        foreach (var s in _slots)
        {
            contentMin += SlotMinHeightFor(s);
        }
        var viewport = _scroll.Bounds.Height;
        var target = Math.Max(viewport, contentMin);
        // Guard against rebinding to the same value — keeps the
        // SizeChanged callback from re-entering itself via layout
        // invalidation. CRITICAL: explicit NaN check first, because
        // NaN comparisons always return false — Math.Abs(NaN - x) > 0.5
        // is false, so without this guard the initial Grid.Height (which
        // is NaN by default) never gets set, and the Grid sizes to
        // content (= content_min) instead of viewport. That's the
        // "shell sits one-third up from the bottom" bug.
        if (double.IsNaN(_grid.Height) || Math.Abs(_grid.Height - target) > 0.5)
        {
            _grid.Height = target;
        }
    }

    private void OnAddClick(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
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
            item.Click += (_, _) => AddSlot(name);
            flyout.Items.Add(item);
        }
        flyout.ShowAt(_addBtn);
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        foreach (var s in _slots)
        {
            s.RequestClose -= OnSlotRequestClose;
            s.PanelChanged -= OnSlotPanelChanged;
            s.Dispose();
        }
        _slots.Clear();
    }
}
