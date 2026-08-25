using System;
using System.Collections.Generic;
using System.Collections.ObjectModel;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Serialization;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Controls.Templates;
using Avalonia.Input;
using Avalonia.Layout;
using Avalonia.Media;
using Avalonia.Threading;

namespace EntityAvalonia.Panels;

// HandlerBrowserPanel renders wb.HandlerBrowserModel — the discovered
// handler set, the selected handler's operations with their input/output
// types, and an execute log. It closes the last console→Avalonia parity
// gap.
//
// **No model logic lives here.** Discovery, selection, spec formatting
// and execution are all in `workbench/handler_model.go`; this file turns
// JSON into controls and clicks into bridge calls. If something here
// starts to look like a decision about WHAT to show rather than HOW, it
// belongs in the model — that is the rule the two renderers exist to
// enforce.
//
// Layout is three bands: handlers on the left, the selected handler's
// operations on the right, and the execute log below both. The custom-
// dispatch row sits under the log because it is the escape hatch, not
// the primary path.
public sealed class HandlerBrowserPanel : UserControl, IDisposable
{
    // P4 (bounded list). Handler counts are small (tens), but the output
    // log grows without bound as a user executes, and an unbounded
    // ItemsSource is how a panel takes the window down. The model keeps
    // its full log; the view renders the tail.
    private const int MaxOutputRows = 2000;

    private readonly long _handle;
    private readonly ListBox _handlerList;
    private readonly ListBox _opList;
    private readonly TextBlock _specLine;
    private readonly ItemsControl _outputView;
    private readonly Button _executeBtn;
    private readonly TextBox _customUri;
    private readonly TextBox _customOp;
    private readonly TextBox _customResource;
    private readonly ObservableCollection<HandlerVm> _handlers = new();
    private readonly ObservableCollection<OpVm> _ops = new();
    private readonly ObservableCollection<OutputVm> _output = new();

    private Bridge.TreeWakeCallback? _wakeCallback;
    private GCHandle _wakeCallbackHandle;
    private bool _disposed;
    private bool _renderQueued;
    // Guards the reentrancy between SelectionChanged and RerenderFromBridge:
    // the render sets SelectedIndex, which fires SelectionChanged, which
    // would call back into the bridge and render again. Without this the
    // panel dispatches a selection round-trip per paint.
    private bool _applyingRender;

    public HandlerBrowserPanel(long peerHandle, IPanelHost? host)
    {
        var openReply = Bridge.TakeString(Bridge.HandlersOpen(peerHandle));
        _handle = ParseHandle(openReply);
        if (_handle < 0)
        {
            Content = new SelectableTextBlock
            {
                Text = $"handler browser open failed: {openReply}",
                Foreground = Brushes.IndianRed,
                Margin = new Thickness(12),
                FontSize = 14,
            };
            return;
        }

        _handlerList = new ListBox
        {
            ItemsSource = _handlers,
            FontFamily = new FontFamily("monospace"),
            FontSize = 13,
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
            ItemTemplate = new FuncDataTemplate<HandlerVm>((vm, _) =>
            {
                var stack = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 8 };
                stack.Children.Add(new TextBlock { Text = vm.Pattern, FontSize = 13 });
                stack.Children.Add(new TextBlock
                {
                    Text = vm.OpCount == 1 ? "1 op" : $"{vm.OpCount} ops",
                    FontSize = 11,
                    Opacity = 0.5,
                    VerticalAlignment = VerticalAlignment.Center,
                });
                return stack;
            }, supportsRecycling: true),
        };
        _handlerList.SelectionChanged += (_, _) =>
        {
            if (_applyingRender || _disposed) return;
            var idx = _handlerList.SelectedIndex;
            if (idx < 0) return;
            Bridge.TakeString(Bridge.HandlersSelectHandler(_handle, idx));
            RerenderFromBridge();
        };

        _opList = new ListBox
        {
            ItemsSource = _ops,
            FontFamily = new FontFamily("monospace"),
            FontSize = 13,
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
            ItemTemplate = new FuncDataTemplate<OpVm>((vm, _) =>
            {
                var stack = new StackPanel { Orientation = Orientation.Vertical };
                stack.Children.Add(new TextBlock { Text = vm.Name, FontSize = 13 });
                if (!string.IsNullOrEmpty(vm.Types))
                {
                    stack.Children.Add(new TextBlock
                    {
                        Text = vm.Types,
                        FontSize = 11,
                        Opacity = 0.5,
                    });
                }
                return stack;
            }, supportsRecycling: true),
        };
        _opList.SelectionChanged += (_, _) =>
        {
            if (_applyingRender || _disposed) return;
            var idx = _opList.SelectedIndex;
            if (idx < 0) return;
            Bridge.TakeString(Bridge.HandlersSelectOperation(_handle, idx));
            RerenderFromBridge();
        };
        // Enter on the operation list executes — the console binding.
        _opList.KeyDown += (_, e) =>
        {
            if (e.Key == Key.Enter)
            {
                ExecuteSelected();
                e.Handled = true;
            }
        };

        _specLine = new TextBlock
        {
            Text = "(no operation selected)",
            FontFamily = new FontFamily("monospace"),
            FontSize = 12,
            Opacity = 0.65,
            Margin = new Thickness(0, 4, 0, 4),
            TextWrapping = TextWrapping.Wrap,
        };

        _executeBtn = new Button
        {
            Content = "Execute",
            FontSize = 12,
            Padding = new Thickness(10, 4),
            Margin = new Thickness(0, 0, 0, 6),
            HorizontalAlignment = HorizontalAlignment.Left,
        };
        _executeBtn.Click += (_, _) => ExecuteSelected();

        _outputView = new ItemsControl
        {
            ItemsSource = _output,
            ItemTemplate = new FuncDataTemplate<OutputVm>((vm, _) => new SelectableTextBlock
            {
                Text = vm.Text,
                FontFamily = new FontFamily("monospace"),
                FontSize = 12,
                Foreground = BrushForKind(vm.Kind),
            }, supportsRecycling: true),
        };

        _customUri = new TextBox { Watermark = "handler uri", FontSize = 12, FontFamily = new FontFamily("monospace") };
        _customOp = new TextBox { Watermark = "op", FontSize = 12, FontFamily = new FontFamily("monospace") };
        _customResource = new TextBox { Watermark = "resource (optional)", FontSize = 12, FontFamily = new FontFamily("monospace") };
        var customBtn = new Button { Content = "Dispatch", FontSize = 12, Padding = new Thickness(10, 4) };
        customBtn.Click += (_, _) => ExecuteCustom();
        foreach (var box in new[] { _customUri, _customOp, _customResource })
        {
            box.KeyDown += (_, e) =>
            {
                if (e.Key == Key.Enter) { ExecuteCustom(); e.Handled = true; }
            };
        }

        var lists = new Grid { ColumnDefinitions = new ColumnDefinitions("*,*") };
        var handlersHeader = Header("handlers");
        DockPanel.SetDock(handlersHeader, Dock.Top);
        var left = new DockPanel { LastChildFill = true, Margin = new Thickness(0, 0, 6, 0) };
        left.Children.Add(handlersHeader);
        left.Children.Add(new ScrollViewer { Content = _handlerList });

        var opsHeader = Header("operations");
        DockPanel.SetDock(opsHeader, Dock.Top);
        var right = new DockPanel { LastChildFill = true };
        right.Children.Add(opsHeader);
        right.Children.Add(new ScrollViewer { Content = _opList });
        Grid.SetColumn(left, 0);
        Grid.SetColumn(right, 1);
        lists.Children.Add(left);
        lists.Children.Add(right);

        var customRow = new Grid
        {
            ColumnDefinitions = new ColumnDefinitions("*,120,*,Auto"),
            Margin = new Thickness(0, 6, 0, 0),
        };
        Grid.SetColumn(_customUri, 0);
        Grid.SetColumn(_customOp, 1);
        Grid.SetColumn(_customResource, 2);
        Grid.SetColumn(customBtn, 3);
        customRow.Children.Add(_customUri);
        customRow.Children.Add(_customOp);
        customRow.Children.Add(_customResource);
        customRow.Children.Add(customBtn);

        // Star-weighted rows are pinned with MinHeight per the PanelStack
        // zero-size lesson: a splitter drag that drives a star row to zero
        // took the process down twice with an identical signature. The
        // pins are structural, not cosmetic.
        var root = new Grid
        {
            RowDefinitions = new RowDefinitions("2*,Auto,Auto,3*,Auto"),
            Margin = new Thickness(10),
        };
        lists.MinHeight = 80;
        _outputView.MinHeight = 40;
        Grid.SetRow(lists, 0);
        Grid.SetRow(_specLine, 1);
        Grid.SetRow(_executeBtn, 2);
        var outputScroll = new ScrollViewer { Content = _outputView, MinHeight = 40 };
        Grid.SetRow(outputScroll, 3);
        Grid.SetRow(customRow, 4);
        root.Children.Add(lists);
        root.Children.Add(_specLine);
        root.Children.Add(_executeBtn);
        root.Children.Add(outputScroll);
        root.Children.Add(customRow);
        Content = root;

        _wakeCallback = OnWakeFromGo;
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        var cbPtr = Marshal.GetFunctionPointerForDelegate(_wakeCallback);
        Bridge.TakeString(Bridge.HandlersRegisterWake(_handle, cbPtr));

        PanelLog.Write("handler-browser", $"Mount h={_handle}");
        RerenderFromBridge();
    }

    // --- Test accessors -------------------------------------------------
    // The house pattern for headless panel tests: expose the state a test
    // needs to assert on, not the controls. A test that reaches into the
    // visual tree pins the layout, and then a cosmetic change reads as a
    // regression.

    public long HandleForTests => _handle;
    public int SelectedHandlerIndexForTests => _handlerList?.SelectedIndex ?? -1;
    public int HandlerCountForTests => _handlers.Count;
    public int OperationCountForTests => _ops.Count;
    public int OutputCountForTests => _output.Count;
    public string SpecLineForTests => _specLine?.Text ?? "";
    public bool ExecuteEnabledForTests => _executeBtn?.IsEnabled ?? false;
    public string HandlerPatternAtForTests(int i) =>
        i >= 0 && i < _handlers.Count ? _handlers[i].Pattern : "";
    public string OperationNameAtForTests(int i) =>
        i >= 0 && i < _ops.Count ? _ops[i].Name : "";
    public void SelectHandlerForTests(int i) => _handlerList.SelectedIndex = i;
    public void ExecuteSelectedForTests() => ExecuteSelected();
    public void ExecuteCustomForTests(string uri, string op, string resource)
    {
        _customUri.Text = uri;
        _customOp.Text = op;
        _customResource.Text = resource;
        ExecuteCustom();
    }

    private void OnWakeFromGo(long handle)
    {
        if (_disposed) return;
        // Coalesce: a burst of handler registrations at boot would
        // otherwise post one render per event.
        if (_renderQueued) return;
        _renderQueued = true;
        Dispatcher.UIThread.Post(() =>
        {
            _renderQueued = false;
            if (_disposed) return;
            RerenderFromBridge();
        });
    }

    private void ExecuteSelected()
    {
        if (_handle < 0 || _disposed) return;
        PanelLog.Write("handler-browser", $"Execute h={_handle}");
        Bridge.TakeString(Bridge.HandlersExecuteSelected(_handle));
        RerenderFromBridge();
    }

    private void ExecuteCustom()
    {
        if (_handle < 0 || _disposed) return;
        var uri = _customUri.Text ?? "";
        var op = _customOp.Text ?? "";
        var resource = _customResource.Text ?? "";
        PanelLog.Write("handler-browser", $"ExecuteCustom h={_handle} uri='{uri}' op='{op}'");
        Bridge.TakeString(Bridge.HandlersExecuteCustom(_handle, uri, op, resource));
        RerenderFromBridge();
    }

    private void RerenderFromBridge()
    {
        if (_handle < 0 || _disposed) return;
        var reply = Bridge.TakeString(Bridge.HandlersRender(_handle));
        RenderDto? dto;
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (!doc.RootElement.TryGetProperty("ok", out var ok) || !ok.GetBoolean())
            {
                _specLine.Text = reply;
                _specLine.Foreground = Brushes.IndianRed;
                return;
            }
            dto = doc.RootElement.GetProperty("result").Deserialize<RenderDto>();
        }
        catch (Exception ex)
        {
            _specLine.Text = $"handler render parse failed: {ex.Message}";
            _specLine.Foreground = Brushes.OrangeRed;
            return;
        }
        if (dto == null) return;

        _applyingRender = true;
        try
        {
            // **Never Clear() a bound collection while its ListBox holds a
            // selection.** Avalonia's SelectionModel re-reads the selected
            // index against the emptied source and throws
            // ArgumentOutOfRangeException from deep inside
            // SelectingItemsControl — nothing in this file appears in the
            // stack. Caught by the X11 smoke driver, which died silently
            // after one iteration; the headless suite missed it because it
            // asserted on the first handler and so never CHANGED the
            // selection. Both list rebuilds below drop the selection first.
            //
            // The handler list additionally only rebuilds when its content
            // actually differs. The discovered set changes when a handler
            // registers — rare — while selection changes are constant, and
            // rebuilding a list on every selection change is churn that
            // buys nothing and costs the scroll position.
            if (HandlersDiffer(dto.Handlers))
            {
                _handlerList.SelectedIndex = -1;
                _handlers.Clear();
                if (dto.Handlers != null)
                {
                    foreach (var h in dto.Handlers)
                    {
                        _handlers.Add(new HandlerVm(h.Pattern, h.Name, h.OpCount));
                    }
                }
            }

            _opList.SelectedIndex = -1;
            _ops.Clear();
            if (dto.Operations != null)
            {
                foreach (var o in dto.Operations)
                {
                    _ops.Add(new OpVm(o.Name, FormatTypes(o.InputType, o.OutputType)));
                }
            }

            _output.Clear();
            if (dto.Output != null)
            {
                int start = Math.Max(0, dto.Output.Count - MaxOutputRows);
                for (int i = start; i < dto.Output.Count; i++)
                {
                    _output.Add(new OutputVm(dto.Output[i].Text, dto.Output[i].Kind));
                }
            }

            if (dto.SelectedHandler >= 0 && dto.SelectedHandler < _handlers.Count)
            {
                _handlerList.SelectedIndex = dto.SelectedHandler;
            }
            if (dto.SelectedOp >= 0 && dto.SelectedOp < _ops.Count)
            {
                _opList.SelectedIndex = dto.SelectedOp;
            }

            _specLine.ClearValue(TextBlock.ForegroundProperty);
            _specLine.Text = string.IsNullOrEmpty(dto.SpecLine)
                ? (_handlers.Count == 0 ? "(no handlers discovered)" : "(no operation selected)")
                : dto.SpecLine;
            _executeBtn.IsEnabled = !string.IsNullOrEmpty(dto.SelectedOpName);
        }
        finally
        {
            _applyingRender = false;
        }
    }

    // HandlersDiffer reports whether the incoming handler set differs from
    // what is already rendered. Compared on pattern + op count: the
    // pattern is the identity and the op count is the only other field
    // shown, so anything else changing is invisible anyway.
    private bool HandlersDiffer(List<HandlerDto>? incoming)
    {
        int n = incoming?.Count ?? 0;
        if (n != _handlers.Count) return true;
        for (int i = 0; i < n; i++)
        {
            if (incoming![i].Pattern != _handlers[i].Pattern) return true;
            if (incoming[i].OpCount != _handlers[i].OpCount) return true;
        }
        return false;
    }

    private static string FormatTypes(string input, string output)
    {
        if (string.IsNullOrEmpty(input) && string.IsNullOrEmpty(output)) return "";
        var parts = new List<string>(2);
        if (!string.IsNullOrEmpty(input)) parts.Add("in: " + input);
        if (!string.IsNullOrEmpty(output)) parts.Add("out: " + output);
        return string.Join("   ", parts);
    }

    private static Control Header(string text) => new TextBlock
    {
        Text = text,
        FontSize = 11,
        Opacity = 0.5,
        Margin = new Thickness(0, 0, 0, 4),
    };

    private static IBrush BrushForKind(string kind) => kind switch
    {
        "error" => Brushes.IndianRed,
        "path" => Brushes.CornflowerBlue,
        "bytes" => Brushes.DarkKhaki,
        _ => Brushes.Gainsboro,
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

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        PanelLog.Write("handler-browser", $"Dispose h={_handle}");
        if (_handle >= 0)
        {
            Bridge.HandlersClose(_handle);
        }
        // Close first, then release the delegate: HandlersClose joins the
        // wake goroutine, so after it returns no callback can be in
        // flight. Freeing the GCHandle before that is the use-after-free
        // shape the crash hunt kept finding.
        _wakeCallback = null;
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
    }

    private sealed class RenderDto
    {
        [JsonPropertyName("handlers")] public List<HandlerDto>? Handlers { get; set; }
        [JsonPropertyName("selected_handler")] public int SelectedHandler { get; set; }
        [JsonPropertyName("selected_op")] public int SelectedOp { get; set; }
        [JsonPropertyName("selected_pattern")] public string SelectedPattern { get; set; } = "";
        [JsonPropertyName("selected_op_name")] public string SelectedOpName { get; set; } = "";
        [JsonPropertyName("spec_line")] public string SpecLine { get; set; } = "";
        [JsonPropertyName("operations")] public List<OpDto>? Operations { get; set; }
        [JsonPropertyName("output")] public List<OutputDto>? Output { get; set; }
    }

    private sealed class HandlerDto
    {
        [JsonPropertyName("pattern")] public string Pattern { get; set; } = "";
        [JsonPropertyName("name")] public string Name { get; set; } = "";
        [JsonPropertyName("op_count")] public int OpCount { get; set; }
    }

    private sealed class OpDto
    {
        [JsonPropertyName("name")] public string Name { get; set; } = "";
        [JsonPropertyName("input_type")] public string InputType { get; set; } = "";
        [JsonPropertyName("output_type")] public string OutputType { get; set; } = "";
    }

    private sealed class OutputDto
    {
        [JsonPropertyName("text")] public string Text { get; set; } = "";
        [JsonPropertyName("kind")] public string Kind { get; set; } = "";
    }

    private sealed record HandlerVm(string Pattern, string Name, int OpCount);
    private sealed record OpVm(string Name, string Types);
    private sealed record OutputVm(string Text, string Kind);
}
