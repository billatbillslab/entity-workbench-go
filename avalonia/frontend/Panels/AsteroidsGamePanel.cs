using System;
using System.Collections.Generic;
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

// AsteroidsGamePanel — the display/input driver for the Asteroids hostable
// compute program: the first heterogeneous, variable-actor-set program, and the
// first panel driven by a DISPLAY-LIST output port.
//
// WHY THIS PANEL EXISTS (and what it proves):
//
// Life and Snake are both grids, so their state IS their display and their
// output port points straight at the state path. Asteroids is the first program
// where state and display come apart — the state is an actor array, the display
// is free-floating vector shapes. Rather than teach a renderer what an asteroid
// looks like, the PROGRAM emits a display list (world-space quads + an opaque
// kind tag) and this panel just draws polylines.
//
// So the test of the whole port argument is right here, in ShapeControl.Render:
// it contains NO asteroid semantics. No positions, no velocities, no radii, no
// sizes, no rotation, no notion of "ship" or "bullet" beyond a colour lookup on
// an opaque integer. It would render Snake, Life, or Doom's automap unchanged.
// If this file ever has to learn what an asteroid IS, the display-list
// recommendation in COMPUTE-ASTEROIDS-PORT-TAXONOMY-2026-07-16.md §4 was wrong.
//
// The INPUT driver is the other finding: Asteroids needs thrust + rotate + fire
// held SIMULTANEOUSLY, which Snake's single last-write-wins direction port
// cannot express. The panel tracks the held-key SET and writes it as a bitmask —
// still a snapshot port, sampled at the tick boundary, no new port kind.
//
// Everything else — the step, the tick clock, the game rules, the display-list
// derivation — lives behind the bridge in wb.AsteroidsGameModel; this panel
// holds NO game logic (workbench-brain discipline).
//
// Standard skeleton: open/registerWake/render/close with a pinned wake delegate
// (P6) and single-flight UI-thread posting. The scene is one custom-drawn
// control (no per-actor UIElement churn; the actor count is bounded by the
// program's own fixed cap, so P4 is satisfied structurally).
public sealed class AsteroidsGamePanel : UserControl, IDisposable, IPanelPreferredHeight
{
    // The scene is a SQUARE world, so it scales with min(width, height). In a
    // default three-slot stack the row is wide but short, which collapsed the
    // world to the slot's leftover height (~135px) and threw away all the width.
    // Ask for enough that the board is legible without a splitter drag; the
    // stack still star-shares above this floor and the ScrollViewer covers the
    // overflow. 520 - chrome leaves a ~400px world.
    public double PreferredSlotMinHeight => 520;

    private readonly long _handle;
    private readonly TextBlock _statusLine;
    private readonly ShapeControl _scene;
    private readonly Button _startPause;

    private Bridge.TreeWakeCallback? _wakeCallback;
    private GCHandle _wakeCallbackHandle; // explicit GC root (P6)
    private bool _renderQueued;
    private bool _disposed;
    private bool _running;

    // The held-key SET — the input port's whole payload. Bits match
    // wb.AsteroidsKey*: 0 left, 1 right, 2 thrust, 3 fire.
    private long _keys;

    private const int KeyLeft = 0;
    private const int KeyRight = 1;
    private const int KeyThrust = 2;
    private const int KeyFire = 3;

    public AsteroidsGamePanel(long peerHandle)
    {
        Focusable = true;

        var openReply = Bridge.TakeString(Bridge.AsteroidsOpen(peerHandle));
        _handle = ParseHandle(openReply);
        if (_handle < 0)
        {
            Content = new SelectableTextBlock
            {
                Text = $"asteroids open failed: {openReply}",
                Foreground = Brushes.IndianRed,
                Margin = new Thickness(12),
                FontSize = 14,
            };
            return;
        }

        var header = new TextBlock
        {
            Text = "Asteroids — a compute program (←/→ or A/D turn · ↑/W thrust · Space fire)",
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
            PanelLog.Write("asteroids", _running ? $"Stop h={_handle}" : $"Start h={_handle}");
            Bridge.TakeString(_running ? Bridge.AsteroidsStop(_handle) : Bridge.AsteroidsStart(_handle));
            Focus();
        };
        var restart = new Button { Content = "Restart", MinWidth = 80 };
        restart.Click += (_, _) =>
        {
            if (_handle < 0) return;
            PanelLog.Write("asteroids", $"Restart h={_handle}");
            _keys = 0;
            Bridge.TakeString(Bridge.AsteroidsRestart(_handle));
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

        _scene = new ShapeControl();
        _scene.PointerPressed += (_, _) => Focus();

        var dock = new DockPanel { LastChildFill = true, Margin = new Thickness(8) };
        DockPanel.SetDock(header, Dock.Top);
        DockPanel.SetDock(_statusLine, Dock.Top);
        DockPanel.SetDock(buttons, Dock.Top);
        dock.Children.Add(header);
        dock.Children.Add(_statusLine);
        dock.Children.Add(buttons);
        dock.Children.Add(_scene);
        Content = dock;

        // Held-key tracking needs BOTH edges — this is what makes the port a
        // SET rather than a stream of presses.
        KeyDown += OnKeyDown;
        KeyUp += OnKeyUp;

        _wakeCallback = OnWakeFromGo;
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        var cbPtr = Marshal.GetFunctionPointerForDelegate(_wakeCallback);
        Bridge.TakeString(Bridge.AsteroidsRegisterWake(_handle, cbPtr));
        PanelLog.Write("asteroids", $"Mount h={_handle}");
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

    private static int BitFor(Key k) => k switch
    {
        Key.Left or Key.A => KeyLeft,
        Key.Right or Key.D => KeyRight,
        Key.Up or Key.W => KeyThrust,
        Key.Space => KeyFire,
        _ => -1,
    };

    private void OnKeyDown(object? sender, KeyEventArgs e)
    {
        int bit = BitFor(e.Key);
        if (bit < 0) return;
        SetKey(bit, true);
        e.Handled = true;
    }

    private void OnKeyUp(object? sender, KeyEventArgs e)
    {
        int bit = BitFor(e.Key);
        if (bit < 0) return;
        SetKey(bit, false);
        e.Handled = true;
    }

    // SetKey is the input driver: update the held set and write the WHOLE set to
    // the snapshot input port. Last-write-wins; the step samples it at the next
    // tick boundary. Writing only on CHANGE (not per tick) is what keeps this a
    // snapshot port rather than an event stream.
    private void SetKey(int bit, bool down)
    {
        if (_handle < 0) return;
        long next = down ? (_keys | (1L << bit)) : (_keys & ~(1L << bit));
        if (next == _keys) return; // key repeat — the set did not change
        _keys = next;
        Bridge.TakeString(Bridge.AsteroidsInput(_handle, _keys));
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
        var reply = Bridge.TakeString(Bridge.AsteroidsRender(_handle));
        AsteroidsFrameDto? dto = null;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                _statusLine.Text = reply;
                _statusLine.Foreground = Brushes.IndianRed;
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<AsteroidsFrameDto>();
        }
        catch (Exception ex)
        {
            _statusLine.Text = $"asteroids render parse failed: {ex.Message}";
            _statusLine.Foreground = Brushes.OrangeRed;
            return;
        }
        if (dto == null) return;

        _running = dto.Running;
        _startPause.Content = _running ? "Pause" : "Start";
        _statusLine.ClearValue(TextBlock.ForegroundProperty);
        var state = dto.Err is { Length: > 0 } ? $"ERROR {dto.Err}"
            : dto.Status == 1 ? "DESTROYED — Restart to play again"
            : _running ? "playing" : "paused";
        int drawables = dto.Quads?.Length ?? 0;
        _statusLine.Text =
            $"score {dto.Score}  ·  actors {drawables}  ·  tick {dto.Ticks}  ·  {state}";
        if (dto.Err is { Length: > 0 })
        {
            _statusLine.Foreground = Brushes.OrangeRed;
        }

        _scene.SetFrame(dto);
    }

    // --- smoke-driver hooks (WB_SMOKE_ASTEROIDS; mirrors SnakeGamePanel) — the
    // driver exercises exactly the surfaces a user would: start the clock, write
    // the held-key port, read status.
    internal void StartForTests()
    {
        if (_handle < 0) return;
        Bridge.TakeString(Bridge.AsteroidsStart(_handle));
    }

    internal void InputForTests(long keys)
    {
        if (_handle < 0) return;
        _keys = keys;
        Bridge.TakeString(Bridge.AsteroidsInput(_handle, keys));
    }

    internal string StatusTextForTests => _statusLine?.Text ?? "(no status)";

    internal int DrawableCountForTests => _scene?.DrawableCount ?? 0;

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        PanelLog.Write("asteroids", $"Dispose h={_handle}");
        if (_handle >= 0)
        {
            Bridge.AsteroidsClose(_handle);
        }
        _wakeCallback = null;
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
    }

    // ShapeControl draws the whole frame in one Render pass — no child controls,
    // no layout churn; InvalidateVisual per frame is the entire update path.
    //
    // READ THIS IF YOU ARE EVALUATING THE DISPLAY-LIST PORT: everything below is
    // a generic polyline renderer. It knows the world extent (to scale), a
    // vertex list, and a colour table keyed by an opaque integer. It does not
    // know what an asteroid is, how big one should be, which way one is facing,
    // or that this is a game. That is the property the port was chosen for.
    private sealed class ShapeControl : Control
    {
        private AsteroidsFrameDto? _frame;

        private static readonly IBrush BgBrush = new SolidColorBrush(Color.FromRgb(16, 18, 24));
        private static readonly IPen BorderPen = new Pen(new SolidColorBrush(Color.FromRgb(48, 52, 62)), 1);

        // Colour by KIND TAG — the only per-program knowledge in this control,
        // and it is a lookup table, not logic. An unknown kind still draws.
        private static readonly IPen[] KindPens =
        {
            new Pen(new SolidColorBrush(Color.FromRgb(90, 96, 110)), 1),   // 0 (unused: free slots aren't emitted)
            new Pen(new SolidColorBrush(Color.FromRgb(150, 240, 130)), 2), // 1 ship
            new Pen(new SolidColorBrush(Color.FromRgb(190, 190, 210)), 1.5),// 2 asteroid
            new Pen(new SolidColorBrush(Color.FromRgb(240, 200, 90)), 2),  // 3 bullet
        };
        private static readonly IPen FallbackPen =
            new Pen(new SolidColorBrush(Color.FromRgb(230, 110, 90)), 1.5);

        public int DrawableCount => _frame?.Quads?.Length ?? 0;

        public void SetFrame(AsteroidsFrameDto frame)
        {
            _frame = frame;
            InvalidateVisual();
        }

        public override void Render(DrawingContext ctx)
        {
            base.Render(ctx);
            var f = _frame;
            ctx.FillRectangle(BgBrush, new Rect(Bounds.Size));
            if (f == null || f.World <= 0 || f.Quads == null) return;

            // Square viewport, centred — the world is square.
            double side = Math.Min(Bounds.Width, Bounds.Height);
            if (side <= 4) return;
            double ox = (Bounds.Width - side) / 2;
            double oy = (Bounds.Height - side) / 2;
            double scale = side / f.World;
            ctx.DrawRectangle(null, BorderPen, new Rect(ox, oy, side, side));

            // Clip to the world square so tiled copies (below) cannot bleed into
            // the panel's chrome.
            using var clip = ctx.PushClip(new Rect(ox, oy, side, side));

            foreach (var q in f.Quads)
            {
                if (q.X == null || q.Y == null || q.X.Length < 2 || q.Y.Length != q.X.Length)
                {
                    continue;
                }
                var pen = q.Kind < (ulong)KindPens.Length ? KindPens[q.Kind] : FallbackPen;

                // WRAP-AROUND TILING. An actor's CENTRE wraps (the program does
                // `mod world`), but its outline is centre + offsets and is NOT
                // wrapped — wrapping each vertex independently would tear the
                // polygon in half, teleporting single corners across the world.
                //
                // So the outline is drawn in an unwrapped local frame, and the
                // renderer tiles it: draw the same shape shifted by 0 / ±world
                // on each axis and keep the copies that touch the viewport. A
                // shape straddling the seam then appears correctly on BOTH
                // edges, which is what the actor physically is on a torus.
                //
                // This is generic — it needs to know the scene wraps (f.Wrap),
                // not what an asteroid is — so it stays inside the display-list
                // contract.
                double minX = q.X[0], maxX = q.X[0], minY = q.Y[0], maxY = q.Y[0];
                for (int i = 1; i < q.X.Length; i++)
                {
                    if (q.X[i] < minX) minX = q.X[i];
                    if (q.X[i] > maxX) maxX = q.X[i];
                    if (q.Y[i] < minY) minY = q.Y[i];
                    if (q.Y[i] > maxY) maxY = q.Y[i];
                }

                foreach (var sx in WrapOffsets(f.Wrap, minX, maxX, f.World))
                {
                    foreach (var sy in WrapOffsets(f.Wrap, minY, maxY, f.World))
                    {
                        var geo = new StreamGeometry();
                        using (var gc = geo.Open())
                        {
                            gc.BeginFigure(
                                new Point(ox + (q.X[0] + sx) * scale, oy + (q.Y[0] + sy) * scale), false);
                            for (int i = 1; i < q.X.Length; i++)
                            {
                                gc.LineTo(new Point(
                                    ox + (q.X[i] + sx) * scale, oy + (q.Y[i] + sy) * scale));
                            }
                            gc.LineTo(new Point(ox + (q.X[0] + sx) * scale, oy + (q.Y[0] + sy) * scale));
                            gc.EndFigure(true);
                        }
                        ctx.DrawGeometry(null, pen, geo);
                    }
                }
            }
        }

        // WrapOffsets yields the shifts needed on one axis: always 0, plus a
        // +world copy when the shape pokes out the low edge and a -world copy
        // when it pokes out the high edge. Non-wrapping scenes get just 0.
        private static IEnumerable<double> WrapOffsets(bool wrap, double lo, double hi, double world)
        {
            yield return 0;
            if (!wrap) yield break;
            if (lo < 0) yield return world;
            if (hi > world) yield return -world;
        }
    }

    public sealed class AsteroidsQuadDto
    {
        [JsonPropertyName("kind")] public ulong Kind { get; set; }
        [JsonPropertyName("x")] public long[]? X { get; set; }
        [JsonPropertyName("y")] public long[]? Y { get; set; }
    }

    public sealed class AsteroidsFrameDto
    {
        [JsonPropertyName("world")] public long World { get; set; }
        // Scene-level: the world is a torus, so outlines tile at the seams.
        [JsonPropertyName("wrap")] public bool Wrap { get; set; }
        [JsonPropertyName("quads")] public AsteroidsQuadDto[]? Quads { get; set; }
        [JsonPropertyName("score")] public ulong Score { get; set; }
        [JsonPropertyName("status")] public ulong Status { get; set; }
        [JsonPropertyName("running")] public bool Running { get; set; }
        [JsonPropertyName("ticks")] public ulong Ticks { get; set; }
        [JsonPropertyName("err")] public string? Err { get; set; }
    }
}
