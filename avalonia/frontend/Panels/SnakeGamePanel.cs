using System;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Serialization;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Input;
using Avalonia.Layout;
using Avalonia.Media;
using Avalonia.Threading;

namespace EntityAvalonia.Panels;

// SnakeGamePanel — the console-tier driver for the Snake hostable
// compute program (the compute-program runtime contract's first GUI
// consumer). The DISPLAY driver reads the output port every wake
// (Bridge.SnakeRender → one frame DTO) and blits it; the INPUT driver
// forwards arrow/WASD keys to the snapshot input port
// (Bridge.SnakeInput). Everything else — the step, the tick clock, the
// game rules — lives behind the bridge in wb.SnakeGameModel; this
// panel holds NO game logic (workbench-brain discipline).
//
// Standard skeleton: open/registerWake/render/close with a pinned wake
// delegate (P6) and single-flight UI-thread posting. The board is one
// custom-drawn control (BoardControl.Render — 12×12 = 144 rects,
// bounded by the program itself, so P4 is satisfied structurally; no
// per-cell UIElement churn, P2 not applicable).
public sealed class SnakeGamePanel : UserControl, IDisposable, IPanelPreferredHeight
{
    // The board is a SQUARE grid, so cell size scales with
    // min(width/cols, height/rows). In a default three-slot stack the row is
    // wide but short, so the board collapsed to the slot's leftover height and
    // threw away all the width. Ask for enough to be legible without a splitter
    // drag; the stack still star-shares above this floor.
    public double PreferredSlotMinHeight => 520;

    private readonly long _handle;
    private readonly TextBlock _statusLine;
    private readonly BoardControl _board;
    private readonly Button _startPause;

    private Bridge.TreeWakeCallback? _wakeCallback;
    private GCHandle _wakeCallbackHandle; // explicit GC root (P6)
    private bool _renderQueued;
    private bool _disposed;
    private bool _running;

    public SnakeGamePanel(long peerHandle)
    {
        Focusable = true;

        var openReply = Bridge.TakeString(Bridge.SnakeOpen(peerHandle));
        _handle = ParseHandle(openReply);
        if (_handle < 0)
        {
            Content = new SelectableTextBlock
            {
                Text = $"snake open failed: {openReply}",
                Foreground = Brushes.IndianRed,
                Margin = new Thickness(12),
                FontSize = 14,
            };
            return;
        }

        var header = new TextBlock
        {
            Text = "Snake — a compute program (arrows / WASD steer)",
            FontWeight = FontWeight.SemiBold,
            FontSize = 14,
            Margin = new Thickness(0, 0, 0, 6),
            Opacity = 0.8,
        };

        _statusLine = new TextBlock
        {
            Text = "(press Start)",
            FontFamily = new FontFamily("monospace"),
            FontSize = 13,
            Opacity = 0.85,
            Margin = new Thickness(0, 0, 0, 6),
        };

        _startPause = new Button { Content = "Start", MinWidth = 80 };
        _startPause.Click += (_, _) =>
        {
            if (_handle < 0) return;
            PanelLog.Write("snake", _running ? $"Stop h={_handle}" : $"Start h={_handle}");
            Bridge.TakeString(_running ? Bridge.SnakeStop(_handle) : Bridge.SnakeStart(_handle));
            Focus();
        };
        var restart = new Button { Content = "Restart", MinWidth = 80 };
        restart.Click += (_, _) =>
        {
            if (_handle < 0) return;
            PanelLog.Write("snake", $"Restart h={_handle}");
            Bridge.TakeString(Bridge.SnakeRestart(_handle));
            Focus();
        };
        var buttons = new StackPanel
        {
            Orientation = Orientation.Horizontal,
            Spacing = 8,
            Margin = new Thickness(0, 0, 0, 8),
        };
        buttons.Children.Add(_startPause);
        buttons.Children.Add(restart);

        _board = new BoardControl();
        // Clicking the board reclaims keyboard focus for steering.
        _board.PointerPressed += (_, _) => Focus();

        var dock = new DockPanel { LastChildFill = true, Margin = new Thickness(8) };
        DockPanel.SetDock(header, Dock.Top);
        DockPanel.SetDock(_statusLine, Dock.Top);
        DockPanel.SetDock(buttons, Dock.Top);
        dock.Children.Add(header);
        dock.Children.Add(_statusLine);
        dock.Children.Add(buttons);
        dock.Children.Add(_board);
        Content = dock;

        KeyDown += OnKeyDown;

        _wakeCallback = OnWakeFromGo;
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        var cbPtr = Marshal.GetFunctionPointerForDelegate(_wakeCallback);
        Bridge.TakeString(Bridge.SnakeRegisterWake(_handle, cbPtr));
        PanelLog.Write("snake", $"Mount h={_handle}");
        RerenderFromBridge();
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

    private void OnKeyDown(object? sender, KeyEventArgs e)
    {
        if (_handle < 0) return;
        long dir = e.Key switch
        {
            Key.Up or Key.W => 0,
            Key.Right or Key.D => 1,
            Key.Down or Key.S => 2,
            Key.Left or Key.A => 3,
            _ => -1,
        };
        if (dir < 0) return;
        // The input driver: one write to the snapshot input port.
        // Last-write-wins; the 180°-reversal guard is IN the program.
        Bridge.TakeString(Bridge.SnakeInput(_handle, dir));
        e.Handled = true;
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
        var reply = Bridge.TakeString(Bridge.SnakeRender(_handle));
        SnakeFrameDto? dto = null;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                _statusLine.Text = reply;
                _statusLine.Foreground = Brushes.IndianRed;
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<SnakeFrameDto>();
        }
        catch (Exception ex)
        {
            _statusLine.Text = $"snake render parse failed: {ex.Message}";
            _statusLine.Foreground = Brushes.OrangeRed;
            return;
        }
        if (dto == null) return;

        _running = dto.Running;
        _startPause.Content = _running ? "Pause" : "Start";
        _statusLine.ClearValue(TextBlock.ForegroundProperty);
        var state = dto.Err is { Length: > 0 } ? $"ERROR {dto.Err}"
            : dto.Status == 1 ? "DEAD — Restart to play again"
            : _running ? "playing" : "paused";
        _statusLine.Text = $"score {dto.Length - 3}  ·  length {dto.Length}  ·  tick {dto.Ticks}  ·  {state}";
        if (dto.Err is { Length: > 0 })
        {
            _statusLine.Foreground = Brushes.OrangeRed;
        }

        _board.SetFrame(dto);
    }

    // --- smoke-driver hooks (WB_SMOKE_SNAKE; mirrors SiteViewPanel's
    // NavigateForTests) — the driver exercises exactly the surfaces a
    // user would: start the clock, write the input port, read status.
    internal void StartForTests()
    {
        if (_handle < 0) return;
        Bridge.TakeString(Bridge.SnakeStart(_handle));
    }

    internal void InputForTests(long dir)
    {
        if (_handle < 0) return;
        Bridge.TakeString(Bridge.SnakeInput(_handle, dir));
    }

    internal string StatusTextForTests => _statusLine?.Text ?? "(no status)";

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        PanelLog.Write("snake", $"Dispose h={_handle}");
        if (_handle >= 0)
        {
            Bridge.SnakeClose(_handle);
        }
        _wakeCallback = null;
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
    }

    // BoardControl draws the whole frame in one Render pass — no child
    // controls, no layout churn; InvalidateVisual per frame is the
    // entire update path.
    private sealed class BoardControl : Control
    {
        private SnakeFrameDto? _frame;

        private static readonly IBrush BgBrush = new SolidColorBrush(Color.FromRgb(24, 26, 30));
        private static readonly IBrush GridBrush = new SolidColorBrush(Color.FromRgb(38, 41, 47));
        private static readonly IBrush BodyBrush = new SolidColorBrush(Color.FromRgb(90, 190, 90));
        private static readonly IBrush HeadBrush = new SolidColorBrush(Color.FromRgb(150, 240, 130));
        private static readonly IBrush DeadBrush = new SolidColorBrush(Color.FromRgb(150, 90, 90));
        private static readonly IBrush FoodBrush = new SolidColorBrush(Color.FromRgb(230, 110, 90));

        public void SetFrame(SnakeFrameDto frame)
        {
            _frame = frame;
            InvalidateVisual();
        }

        public override void Render(DrawingContext ctx)
        {
            base.Render(ctx);
            var f = _frame;
            ctx.FillRectangle(BgBrush, new Rect(Bounds.Size));
            if (f == null || f.Width <= 0 || f.Height <= 0 || f.Cells == null) return;

            double cell = Math.Min(Bounds.Width / f.Width, Bounds.Height / f.Height);
            if (cell <= 1) return;
            double ox = (Bounds.Width - cell * f.Width) / 2;
            double oy = (Bounds.Height - cell * f.Height) / 2;
            var pad = cell * 0.08;

            var body = f.Status == 1 ? DeadBrush : BodyBrush;
            var head = f.Status == 1 ? DeadBrush : HeadBrush;
            for (int y = 0; y < f.Height; y++)
            {
                for (int x = 0; x < f.Width; x++)
                {
                    int i = y * f.Width + x;
                    var r = new Rect(ox + x * cell + pad, oy + y * cell + pad,
                        cell - 2 * pad, cell - 2 * pad);
                    if (i < f.Cells.Length && f.Cells[i] > 0)
                    {
                        ctx.FillRectangle(i == (int)f.Head ? head : body, r);
                    }
                    else if (i == (int)f.Food)
                    {
                        ctx.FillRectangle(FoodBrush, r);
                    }
                    else
                    {
                        ctx.FillRectangle(GridBrush, r);
                    }
                }
            }
        }
    }

    public sealed class SnakeFrameDto
    {
        [JsonPropertyName("width")] public int Width { get; set; }
        [JsonPropertyName("height")] public int Height { get; set; }
        [JsonPropertyName("cells")] public ulong[]? Cells { get; set; }
        [JsonPropertyName("head")] public ulong Head { get; set; }
        [JsonPropertyName("food")] public ulong Food { get; set; }
        [JsonPropertyName("length")] public ulong Length { get; set; }
        [JsonPropertyName("status")] public ulong Status { get; set; }
        [JsonPropertyName("running")] public bool Running { get; set; }
        [JsonPropertyName("ticks")] public ulong Ticks { get; set; }
        [JsonPropertyName("err")] public string? Err { get; set; }
    }
}
