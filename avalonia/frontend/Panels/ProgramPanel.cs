using System;
using System.Collections.Generic;
using System.Linq;
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

// ProgramPanel — the generic driver for ANY hostable compute program.
//
// This ONE class replaces LifeGamePanel + SnakeGamePanel + AsteroidsGamePanel.
// It is constructed with a program NAME, and that name is used for exactly one
// thing: the authoring call. After that it never appears again — the panel binds
// SHAPES (text, display-list, key-set, direction) declared by the descriptor and
// has no idea what it is showing.
//
//   ProgramAuthor(peer, name) -> descriptor path    ; per-program, runs once
//   ProgramMount(peer, path)  -> handle             ; generic, runs always
//
// Grep this file for "life", "snake" or "asteroid" outside the header: the only
// hits are the registration names passed in from Program.cs. There is no branch
// on any of them. That is the claim.
//
// **Why this is the rung and not a refactor.** The three panels it replaces were
// near-identical, differing only in the per-program knowledge they carried: Life
// knew 0/1 meant dead/alive, Snake knew its cell encoding, Asteroids knew what a
// quad was. That knowledge did not get deleted — it moved into compute
// projections in the tree (workbench/program_authoring.go), where a Rust host
// gets it by fetching the program. What is left here is a driver, and drivers
// are per-host by design.
//
// Standard skeleton per GUIDE-AVALONIA-PANEL-PATTERNS: open/registerWake/render/
// close, pinned wake delegate (P6), single-flight UI-thread posting (P3), one
// custom-drawn control per shape (P2/P4 — no per-cell UIElement churn).
public sealed class ProgramPanel : UserControl, IDisposable, IPanelPreferredHeight
{
    // Same reasoning as the panels this replaces: the drawn surface is roughly
    // square, so in a short wide slot it would collapse to the leftover height
    // and throw away the width.
    public double PreferredSlotMinHeight => 520;

    private readonly string _programName;
    private readonly long _handle = -1;
    private readonly TextBlock _statusLine;
    private readonly Panel _stage;
    private readonly Button _startPause;

    // Shape name -> the view driving it. Built lazily from the frame's declared
    // shapes, never from the program's identity.
    private readonly Dictionary<string, IShapeView> _views = new();

    private Bridge.TreeWakeCallback? _wakeCallback;
    private GCHandle _wakeCallbackHandle; // explicit GC root (P6)
    private bool _renderQueued;
    private bool _disposed;
    private bool _running;

    // Held-key state for `key-set` ports. The driver ORs bits; the program
    // decides what they mean.
    private ulong _heldKeys;

    public ProgramPanel(long peerHandle, string programName)
    {
        _programName = programName;

        var authorReply = Bridge.TakeString(Bridge.ProgramAuthor(peerHandle, programName));
        var descriptor = ParseString(authorReply, "descriptor");
        if (descriptor == null)
        {
            Content = Failed($"author failed: {authorReply}");
            return;
        }

        var mountReply = Bridge.TakeString(Bridge.ProgramMount(peerHandle, descriptor));
        _handle = ParseHandle(mountReply);
        if (_handle < 0)
        {
            // A refused mount is a SUCCESS of the admission contract, not a
            // crash: a host that cannot drive a declared shape says so and
            // renders nothing, rather than half-drawing. Surface the reason.
            Content = Failed($"mount refused: {mountReply}");
            return;
        }

        var header = new TextBlock
        {
            Text = $"{programName} — mounted from a descriptor ({descriptor})",
            FontWeight = FontWeight.SemiBold,
            FontSize = 13,
            Margin = new Thickness(0, 0, 0, 6),
            Opacity = 0.8,
            TextTrimming = TextTrimming.CharacterEllipsis,
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
            PanelLog.Write("program", _running ? $"Stop h={_handle}" : $"Start h={_handle}");
            Bridge.TakeString(_running ? Bridge.ProgramStop(_handle) : Bridge.ProgramStart(_handle));
        };
        var restart = new Button { Content = "Restart", MinWidth = 80 };
        restart.Click += (_, _) =>
        {
            if (_handle < 0) return;
            PanelLog.Write("program", $"Restart h={_handle}");
            Bridge.TakeString(Bridge.ProgramRestart(_handle));
        };
        var buttons = new StackPanel
        {
            Orientation = Orientation.Horizontal,
            Spacing = 8,
            Margin = new Thickness(0, 0, 0, 8),
        };
        buttons.Children.Add(_startPause);
        buttons.Children.Add(restart);

        _stage = new Panel();

        var dock = new DockPanel { LastChildFill = true, Margin = new Thickness(8) };
        DockPanel.SetDock(header, Dock.Top);
        DockPanel.SetDock(_statusLine, Dock.Top);
        DockPanel.SetDock(buttons, Dock.Top);
        dock.Children.Add(header);
        dock.Children.Add(_statusLine);
        dock.Children.Add(buttons);
        dock.Children.Add(_stage);
        Content = dock;

        Focusable = true;
        KeyDown += OnKeyDown;
        KeyUp += OnKeyUp;

        _wakeCallback = OnWakeFromGo;
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        var cbPtr = Marshal.GetFunctionPointerForDelegate(_wakeCallback);
        Bridge.TakeString(Bridge.ProgramRegisterWake(_handle, cbPtr));
        PanelLog.Write("program", $"Mount {programName} h={_handle} desc={descriptor}");
        RerenderFromBridge();
    }

    private static Control Failed(string msg) => new SelectableTextBlock
    {
        Text = msg,
        Foreground = Brushes.IndianRed,
        Margin = new Thickness(12),
        FontSize = 14,
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

    private static string? ParseString(string envelope, string field)
    {
        try
        {
            using var doc = JsonDocument.Parse(envelope);
            var root = doc.RootElement;
            if (root.TryGetProperty("ok", out var ok) && ok.GetBoolean()
                && root.TryGetProperty(field, out var v))
            {
                return v.GetString();
            }
        }
        catch { }
        return null;
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

    private FrameDto? _lastFrame;

    private void RerenderFromBridge()
    {
        if (_handle < 0) return;
        var reply = Bridge.TakeString(Bridge.ProgramRender(_handle));
        FrameDto? dto = null;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                _statusLine.Text = reply;
                _statusLine.Foreground = Brushes.IndianRed;
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<FrameDto>();
        }
        catch (Exception ex)
        {
            _statusLine.Text = $"program render parse failed: {ex.Message}";
            _statusLine.Foreground = Brushes.OrangeRed;
            return;
        }
        if (dto == null) return;
        _lastFrame = dto;

        _running = dto.Running;
        _startPause.Content = _running ? "Pause" : "Start";
        _statusLine.ClearValue(TextBlock.ForegroundProperty);

        // The host's run-state, not any program's. A program's own notion of
        // "dead" is program state and lives in its output port — the panel
        // cannot and should not name it. That is a real product regression
        // against the legacy panels and it is reported, not hidden.
        var state = dto.Err is { Length: > 0 } ? $"ERROR {dto.Err}"
            : dto.Status == 2 ? "FAULTED"
            : _running ? "running" : "paused";
        var shapes = dto.Ports == null ? "" : string.Join(", ", dto.Ports.Values.Select(p => p.Shape));
        _statusLine.Text = $"tick {dto.Ticks}  ·  {state}  ·  shapes: {shapes}";
        if (dto.Err is { Length: > 0 }) _statusLine.Foreground = Brushes.OrangeRed;

        if (dto.Ports == null) return;
        foreach (var port in dto.Ports.Values)
        {
            if (!_views.TryGetValue(port.Shape, out var view))
            {
                view = MakeView(port.Shape);
                if (view == null)
                {
                    // No driver for a declared shape. Admission should have
                    // caught this at Mount, so reaching here means the bridge
                    // and this panel disagree about what is supported.
                    _statusLine.Text = $"no driver for shape {port.Shape}";
                    _statusLine.Foreground = Brushes.IndianRed;
                    continue;
                }
                _views[port.Shape] = view;
                _stage.Children.Add(view.Control);
            }
            view.SetPort(port);
        }
    }

    // MakeView is the shape-driver registry, C# side. Add a shape -> add a case
    // here and a view class; nothing else in this file changes, and no existing
    // program is touched. That is the vocabulary being extensible by a rule.
    private static IShapeView? MakeView(string shape) => shape switch
    {
        "text" => new TextShapeView(),
        "display-list" => new DisplayListShapeView(),
        _ => null,
    };

    // --- Input: bound by SHAPE, never by program ------------------------
    //
    // For `key-set`, scene.keymap declares bit -> semantic action. This table
    // maps the ACTION NAME to a physical key. So the driver learns from the
    // descriptor that bit 2 is "thrust" and binds it to Up — without ever
    // learning that thrust belongs to a ship.
    private static readonly Dictionary<string, Key> ActionKeys = new()
    {
        ["left"] = Key.Left,
        ["right"] = Key.Right,
        ["thrust"] = Key.Up,
        ["fire"] = Key.Space,
        ["up"] = Key.Up,
        ["down"] = Key.Down,
    };

    // The `direction` shape's enum is fixed BY THE SHAPE (wb DirUp/Right/Down/
    // Left), which is what lets this be a blind mapping rather than a per-game
    // one.
    private static readonly Dictionary<Key, long> DirectionKeys = new()
    {
        [Key.Up] = 0, [Key.Right] = 1, [Key.Down] = 2, [Key.Left] = 3,
    };

    private void OnKeyDown(object? sender, KeyEventArgs e)
    {
        if (_handle < 0 || _lastFrame?.Inputs == null) return;
        foreach (var input in _lastFrame.Inputs)
        {
            switch (input.Shape)
            {
                case "direction":
                    if (DirectionKeys.TryGetValue(e.Key, out var dir))
                    {
                        Bridge.TakeString(Bridge.ProgramInputDirection(_handle, input.Name, dir));
                        e.Handled = true;
                    }
                    break;

                case "key-set":
                    var bit = BitForKey(input, e.Key);
                    if (bit >= 0)
                    {
                        _heldKeys |= 1UL << bit;
                        Bridge.TakeString(Bridge.ProgramInputKeys(_handle, input.Name, (long)_heldKeys));
                        e.Handled = true;
                    }
                    break;
            }
        }
    }

    private void OnKeyUp(object? sender, KeyEventArgs e)
    {
        if (_handle < 0 || _lastFrame?.Inputs == null) return;
        foreach (var input in _lastFrame.Inputs)
        {
            if (input.Shape != "key-set") continue;
            var bit = BitForKey(input, e.Key);
            if (bit >= 0)
            {
                _heldKeys &= ~(1UL << bit);
                Bridge.TakeString(Bridge.ProgramInputKeys(_handle, input.Name, (long)_heldKeys));
                e.Handled = true;
            }
        }
    }

    // BitForKey resolves a physical key to a bit via the port's declared keymap.
    // Returns -1 when the key is not bound.
    private static int BitForKey(InputDto input, Key key)
    {
        if (input.Scene == null) return -1;
        if (!input.Scene.TryGetValue("keymap", out var kmEl)) return -1;
        try
        {
            foreach (var entry in kmEl.EnumerateObject())
            {
                var action = entry.Value.GetString();
                if (action != null && ActionKeys.TryGetValue(action, out var k) && k == key
                    && int.TryParse(entry.Name, out var bit))
                {
                    return bit;
                }
            }
        }
        catch { }
        return -1;
    }

    // --- smoke-driver hooks (mirrors the legacy panels' StartForTests) ---
    internal void StartForTests()
    {
        if (_handle < 0) return;
        Bridge.TakeString(Bridge.ProgramStart(_handle));
    }

    internal string StatusTextForTests => _statusLine?.Text ?? "(no status)";
    internal string ProgramNameForTests => _programName;

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        PanelLog.Write("program", $"Dispose {_programName} h={_handle}");
        if (_handle >= 0)
        {
            Bridge.ProgramClose(_handle);
        }
        _wakeCallback = null;
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
    }

    // --- Shape views -----------------------------------------------------

    private interface IShapeView
    {
        Control Control { get; }
        void SetPort(PortDto port);
    }

    // TextShapeView drives the `text` shape: a character grid.
    //
    // Lineage: teletype -> glass TTY -> VT100, the oldest interface there is.
    // This ONE view drives both Life and Snake, which is the cheapest possible
    // demonstration that the vocabulary is a framework rather than a game list:
    // it renders code points and has no concept of a cell, a snake or a rule.
    private sealed class TextShapeView : IShapeView
    {
        private readonly GridControl _ctl = new();
        public Control Control => _ctl;
        public void SetPort(PortDto port) => _ctl.SetFrame(port.Text);

        private sealed class GridControl : Control
        {
            private TextFrameDto? _frame;
            private static readonly IBrush BgBrush = new SolidColorBrush(Color.FromRgb(24, 26, 30));
            private static readonly IBrush FgBrush = new SolidColorBrush(Color.FromRgb(120, 200, 235));
            private static readonly Typeface Mono = new(new FontFamily("monospace"));

            public void SetFrame(TextFrameDto? f) { _frame = f; InvalidateVisual(); }

            public override void Render(DrawingContext ctx)
            {
                base.Render(ctx);
                ctx.FillRectangle(BgBrush, new Rect(Bounds.Size));
                var f = _frame;
                if (f == null || f.Cols <= 0 || f.Rows <= 0 || f.Cells == null) return;

                // Monospace: one cell = one advance. Size the glyph to fit the
                // smaller axis so the grid stays square-ish in any slot.
                int cols = (int)f.Cols;
                int rows = (int)f.Rows;

                // SQUARE cells. Sizing the glyph by min(cw,ch) while positioning
                // by cw/ch independently stretches the grid — a character grid
                // must stay uniform, or a Life glider visibly skews.
                double cell = Math.Min(Bounds.Width / cols, Bounds.Height / rows);
                if (cell <= 2) return;
                double ox = (Bounds.Width - cell * cols) / 2;
                double oy = (Bounds.Height - cell * rows) / 2;

                for (int y = 0; y < rows; y++)
                {
                    for (int x = 0; x < cols; x++)
                    {
                        int i = y * cols + x;
                        if (i >= f.Cells.Length) continue;
                        var ch32 = (int)f.Cells[i];
                        if (ch32 == ' ' || ch32 == 0) continue;
                        var text = new FormattedText(
                            char.ConvertFromUtf32(ch32),
                            System.Globalization.CultureInfo.InvariantCulture,
                            FlowDirection.LeftToRight, Mono, cell * 0.95, FgBrush);
                        // Centre each glyph in its cell: a monospace advance is
                        // narrower than the em box, so left-aligning leaves the
                        // column visually adrift from the grid.
                        ctx.DrawText(text, new Point(
                            ox + x * cell + (cell - text.Width) / 2,
                            oy + y * cell + (cell - text.Height) / 2));
                    }
                }
            }
        }
    }

    // DisplayListShapeView drives the `display-list` shape: polylines with kind
    // tags.
    //
    // Lineage: the vector display. Resolution-independent and O(actors). It
    // draws segments and reads scene.wrap / scene.bounds; it does not know what
    // an asteroid is, and kind tags are colour indices, not object types.
    private sealed class DisplayListShapeView : IShapeView
    {
        private readonly VectorControl _ctl = new();
        public Control Control => _ctl;
        public void SetPort(PortDto port)
        {
            double bounds = 0;
            bool wrap = false;
            if (port.Scene != null)
            {
                if (port.Scene.TryGetValue("bounds", out var b) && b.TryGetDouble(out var bv)) bounds = bv;
                if (port.Scene.TryGetValue("wrap", out var w)
                    && (w.ValueKind == JsonValueKind.True || w.ValueKind == JsonValueKind.False))
                {
                    wrap = w.GetBoolean();
                }
            }
            _ctl.SetFrame(port.DisplayList, bounds, wrap);
        }

        private sealed class VectorControl : Control
        {
            private DisplayListDto? _dl;
            private double _bounds;
            private bool _wrap;

            private static readonly IBrush BgBrush = new SolidColorBrush(Color.FromRgb(10, 12, 16));
            // Kind tags are colour indices. The driver does not know what kind 0
            // IS; it knows kind 0 draws in the first colour.
            private static readonly IPen[] KindPens =
            {
                new Pen(new SolidColorBrush(Color.FromRgb(140, 220, 255)), 1.6),
                new Pen(new SolidColorBrush(Color.FromRgb(200, 200, 210)), 1.2),
                new Pen(new SolidColorBrush(Color.FromRgb(255, 210, 120)), 1.4),
                new Pen(new SolidColorBrush(Color.FromRgb(255, 120, 140)), 1.4),
            };

            public void SetFrame(DisplayListDto? dl, double bounds, bool wrap)
            {
                _dl = dl; _bounds = bounds; _wrap = wrap;
                InvalidateVisual();
            }

            public override void Render(DrawingContext ctx)
            {
                base.Render(ctx);
                ctx.FillRectangle(BgBrush, new Rect(Bounds.Size));
                var d = _dl;
                if (d?.Kinds == null || d.X0 == null || _bounds <= 0) return;

                double scale = Math.Min(Bounds.Width, Bounds.Height) / _bounds;
                double ox = (Bounds.Width - _bounds * scale) / 2;
                double oy = (Bounds.Height - _bounds * scale) / 2;
                double span = _bounds * scale;

                // CLIP to the WORLD square, not to the control.
                //
                // Two reasons, and the second is the interesting one. First, the
                // wrap tiles below are drawn a full world-span away, so without
                // any clip they paint over the header and the neighbouring
                // panels. Second — and this is what the screenshot caught —
                // clipping to the CONTROL is not enough: the world is square
                // (scene.bounds is one number) but the slot is wide, so a tile a
                // full span away still lands inside a 900px-wide control and the
                // ship visibly appeared three times. scene.bounds tells the
                // driver how big the world is; showing exactly one of it is the
                // driver's job.
                var world = new Rect(ox, oy, span, span);
                using var clip = ctx.PushClip(world);

                for (int i = 0; i < d.Kinds.Length; i++)
                {
                    var pen = KindPens[d.Kinds[i] % (ulong)KindPens.Length];
                    var quad = new[]
                    {
                        new Point(ox + d.X0[i] * scale, oy + d.Y0![i] * scale),
                        new Point(ox + d.X1![i] * scale, oy + d.Y1![i] * scale),
                        new Point(ox + d.X2![i] * scale, oy + d.Y2![i] * scale),
                        new Point(ox + d.X3![i] * scale, oy + d.Y3![i] * scale),
                    };
                    DrawClosed(ctx, pen, quad, 0, 0);

                    // scene.wrap: the world is a TORUS. An actor's centre wraps
                    // but its outline is centre+offsets and is deliberately NOT
                    // wrapped per-vertex — wrapping vertices individually would
                    // tear the polygon. So the renderer tiles the whole outline
                    // at the seams instead. It learns the SPACE wraps; it never
                    // learns what is moving through it. This is the field that
                    // proved scene properties are necessary at all.
                    if (!_wrap) continue;
                    foreach (var (dx, dy) in Tiles)
                    {
                        DrawClosed(ctx, pen, quad, dx * span, dy * span);
                    }
                }
            }

            private static void DrawClosed(DrawingContext ctx, IPen pen, Point[] pts, double dx, double dy)
            {
                for (int k = 0; k < pts.Length; k++)
                {
                    var a = pts[k];
                    var b = pts[(k + 1) % pts.Length]; // closed: last -> first
                    ctx.DrawLine(pen,
                        new Point(a.X + dx, a.Y + dy),
                        new Point(b.X + dx, b.Y + dy));
                }
            }

            // The eight neighbours; (0,0) is drawn separately as the real one.
            private static readonly (int, int)[] Tiles =
            {
                (-1, -1), (0, -1), (1, -1),
                (-1, 0), (1, 0),
                (-1, 1), (0, 1), (1, 1),
            };
        }
    }

    // --- DTOs (mirror avalonia/bridge/program.go) ------------------------

    public sealed class FrameDto
    {
        [JsonPropertyName("ports")] public Dictionary<string, PortDto>? Ports { get; set; }
        [JsonPropertyName("inputs")] public List<InputDto>? Inputs { get; set; }
        [JsonPropertyName("status")] public int Status { get; set; }
        [JsonPropertyName("ticks")] public ulong Ticks { get; set; }
        [JsonPropertyName("running")] public bool Running { get; set; }
        [JsonPropertyName("err")] public string? Err { get; set; }
    }

    public sealed class PortDto
    {
        [JsonPropertyName("name")] public string Name { get; set; } = "";
        [JsonPropertyName("shape")] public string Shape { get; set; } = "";
        [JsonPropertyName("scene")] public Dictionary<string, JsonElement>? Scene { get; set; }
        [JsonPropertyName("text")] public TextFrameDto? Text { get; set; }
        [JsonPropertyName("displayList")] public DisplayListDto? DisplayList { get; set; }
    }

    public sealed class InputDto
    {
        [JsonPropertyName("name")] public string Name { get; set; } = "";
        [JsonPropertyName("shape")] public string Shape { get; set; } = "";
        [JsonPropertyName("scene")] public Dictionary<string, JsonElement>? Scene { get; set; }
    }

    public sealed class TextFrameDto
    {
        [JsonPropertyName("cols")] public ulong Cols { get; set; }
        [JsonPropertyName("rows")] public ulong Rows { get; set; }
        [JsonPropertyName("cells")] public ulong[]? Cells { get; set; }
    }

    public sealed class DisplayListDto
    {
        [JsonPropertyName("kinds")] public ulong[]? Kinds { get; set; }
        [JsonPropertyName("x0")] public long[]? X0 { get; set; }
        [JsonPropertyName("y0")] public long[]? Y0 { get; set; }
        [JsonPropertyName("x1")] public long[]? X1 { get; set; }
        [JsonPropertyName("y1")] public long[]? Y1 { get; set; }
        [JsonPropertyName("x2")] public long[]? X2 { get; set; }
        [JsonPropertyName("y2")] public long[]? Y2 { get; set; }
        [JsonPropertyName("x3")] public long[]? X3 { get; set; }
        [JsonPropertyName("y3")] public long[]? Y3 { get; set; }
    }
}
