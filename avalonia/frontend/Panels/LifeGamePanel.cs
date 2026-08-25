using System;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Serialization;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Layout;
using Avalonia.Media;
using Avalonia.Threading;

namespace EntityAvalonia.Panels;

// LifeGamePanel — the console-tier driver for the Conway's Life hostable
// compute program (wb.LifeGameModel). The DISPLAY driver reads the
// output port every wake (Bridge.LifeRender → one frame DTO) and blits
// it. There is no input driver: Life is a closed program, so this panel
// is display-only — the contrast with SnakeGamePanel is the point.
// Everything else — the step, the tick clock, the B3/S23 rule — lives
// behind the bridge in wb.LifeGameModel; this panel holds NO game logic
// (workbench-brain discipline).
//
// Standard skeleton: open/registerWake/render/close with a pinned wake
// delegate (P6) and single-flight UI-thread posting. The board is one
// custom-drawn control (BoardControl.Render — 16×16 = 256 rects,
// bounded by the program itself, so P4 is satisfied structurally; no
// per-cell UIElement churn, P2 not applicable).
public sealed class LifeGamePanel : UserControl, IDisposable
{
    private readonly long _handle;
    private readonly TextBlock _statusLine;
    private readonly BoardControl _board;
    private readonly Button _startPause;

    private Bridge.TreeWakeCallback? _wakeCallback;
    private GCHandle _wakeCallbackHandle; // explicit GC root (P6)
    private bool _renderQueued;
    private bool _disposed;
    private bool _running;

    public LifeGamePanel(long peerHandle)
    {
        var openReply = Bridge.TakeString(Bridge.LifeOpen(peerHandle));
        _handle = ParseHandle(openReply);
        if (_handle < 0)
        {
            Content = new SelectableTextBlock
            {
                Text = $"life open failed: {openReply}",
                Foreground = Brushes.IndianRed,
                Margin = new Thickness(12),
                FontSize = 14,
            };
            return;
        }

        var header = new TextBlock
        {
            Text = "Conway's Life — a compute program (no input; watch it run)",
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
            PanelLog.Write("life", _running ? $"Stop h={_handle}" : $"Start h={_handle}");
            Bridge.TakeString(_running ? Bridge.LifeStop(_handle) : Bridge.LifeStart(_handle));
        };
        var restart = new Button { Content = "New soup", MinWidth = 80 };
        restart.Click += (_, _) =>
        {
            if (_handle < 0) return;
            PanelLog.Write("life", $"Restart h={_handle}");
            Bridge.TakeString(Bridge.LifeRestart(_handle));
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

        var dock = new DockPanel { LastChildFill = true, Margin = new Thickness(8) };
        DockPanel.SetDock(header, Dock.Top);
        DockPanel.SetDock(_statusLine, Dock.Top);
        DockPanel.SetDock(buttons, Dock.Top);
        dock.Children.Add(header);
        dock.Children.Add(_statusLine);
        dock.Children.Add(buttons);
        dock.Children.Add(_board);
        Content = dock;

        _wakeCallback = OnWakeFromGo;
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        var cbPtr = Marshal.GetFunctionPointerForDelegate(_wakeCallback);
        Bridge.TakeString(Bridge.LifeRegisterWake(_handle, cbPtr));
        PanelLog.Write("life", $"Mount h={_handle}");
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
        var reply = Bridge.TakeString(Bridge.LifeRender(_handle));
        LifeFrameDto? dto = null;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                _statusLine.Text = reply;
                _statusLine.Foreground = Brushes.IndianRed;
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<LifeFrameDto>();
        }
        catch (Exception ex)
        {
            _statusLine.Text = $"life render parse failed: {ex.Message}";
            _statusLine.Foreground = Brushes.OrangeRed;
            return;
        }
        if (dto == null) return;

        _running = dto.Running;
        _startPause.Content = _running ? "Pause" : "Start";
        _statusLine.ClearValue(TextBlock.ForegroundProperty);
        // Status mirrors wb LifeRunning/LifeExtinct/LifeStillLife. Both
        // stops are fixed points; the text names which one, because
        // "stopped" alone would hide the difference.
        var state = dto.Err is { Length: > 0 } ? $"ERROR {dto.Err}"
            : dto.Status == 1 ? "EXTINCT — New soup to reseed"
            : dto.Status == 2 ? "STILL LIFE — New soup to reseed"
            : _running ? "running" : "paused";
        _statusLine.Text = $"gen {dto.Generation}  ·  population {dto.Population}  ·  {state}";
        if (dto.Err is { Length: > 0 })
        {
            _statusLine.Foreground = Brushes.OrangeRed;
        }

        _board.SetFrame(dto);
    }

    // --- smoke-driver hooks (WB_SMOKE_LIFE; mirrors SnakeGamePanel's
    // StartForTests) — the driver exercises exactly the surfaces a user
    // would: start the clock, read status.
    internal void StartForTests()
    {
        if (_handle < 0) return;
        Bridge.TakeString(Bridge.LifeStart(_handle));
    }

    internal string StatusTextForTests => _statusLine?.Text ?? "(no status)";

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        PanelLog.Write("life", $"Dispose h={_handle}");
        if (_handle >= 0)
        {
            Bridge.LifeClose(_handle);
        }
        _wakeCallback = null;
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
    }

    // BoardControl draws the whole frame in one Render pass — no child
    // controls, no layout churn; InvalidateVisual per frame is the
    // entire update path.
    private sealed class BoardControl : Control
    {
        private LifeFrameDto? _frame;

        private static readonly IBrush BgBrush = new SolidColorBrush(Color.FromRgb(24, 26, 30));
        private static readonly IBrush DeadBrush = new SolidColorBrush(Color.FromRgb(38, 41, 47));
        private static readonly IBrush AliveBrush = new SolidColorBrush(Color.FromRgb(120, 200, 235));
        private static readonly IBrush FrozenBrush = new SolidColorBrush(Color.FromRgb(150, 150, 160));

        public void SetFrame(LifeFrameDto frame)
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

            // A frozen board (still life) greys out — the run reached a
            // fixed point rather than merely paused.
            var alive = f.Status == 2 ? FrozenBrush : AliveBrush;
            for (int y = 0; y < f.Height; y++)
            {
                for (int x = 0; x < f.Width; x++)
                {
                    int i = y * f.Width + x;
                    var r = new Rect(ox + x * cell + pad, oy + y * cell + pad,
                        cell - 2 * pad, cell - 2 * pad);
                    ctx.FillRectangle(
                        i < f.Cells.Length && f.Cells[i] == 1 ? alive : DeadBrush, r);
                }
            }
        }
    }

    public sealed class LifeFrameDto
    {
        [JsonPropertyName("width")] public int Width { get; set; }
        [JsonPropertyName("height")] public int Height { get; set; }
        [JsonPropertyName("cells")] public ulong[]? Cells { get; set; }
        [JsonPropertyName("population")] public ulong Population { get; set; }
        [JsonPropertyName("status")] public ulong Status { get; set; }
        [JsonPropertyName("running")] public bool Running { get; set; }
        [JsonPropertyName("generation")] public ulong Generation { get; set; }
        [JsonPropertyName("err")] public string? Err { get; set; }
    }
}
