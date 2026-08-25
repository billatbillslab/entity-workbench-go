using System;
using System.Collections.Generic;
using System.Collections.ObjectModel;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Serialization;
using Avalonia;
using Avalonia.Controls;
using Avalonia.Controls.Templates;
using Avalonia.Layout;
using Avalonia.Media;
using Avalonia.Threading;

namespace EntityAvalonia.Panels;

// PublisherVerifyPanel — point it at a published origin and it walks the
// whole CDN-corridor chain: manifest -> signature -> CHAMP trie walk from
// the signed root -> leaves -> enumerate -> absent control.
//
// **It renders the CHAIN, not the site.** SiteViewPanel (beside it) is
// the reader's surface: pages, links, chrome. This is the operator's
// inspector, and the difference is the point of having both. A reader
// asks "what does this site say"; an operator asks "is this origin
// serving what it signed, and which link of that claim is load-bearing".
// `entity-browser-rust` owns the first question for this cohort. This
// panel is our answer to the second, and the two UX shapes are the
// cross-implementation contrast we are actually trying to learn from.
//
// **Why every green step carries a `proves` line.** Five of the six
// steps in this chain are satisfiable by an origin that is lying — an
// origin serving a correctly-signed root that commits to nothing passes
// layout, manifest, signature, and every per-leaf fetch anyone makes.
// `entity-core-go` shipped exactly that for a week (fixed at their
// `dabd076`) and every per-leaf consumer in the cohort reported green.
// A UI that collapses this to one tick teaches an operator that
// "verified" is one fact. It is six, and one of them is a *moment*
// rather than a state — which is what the freshness line under the
// verdict says.
//
// No peer handle: a Mode A2 consumer is not a peer. The panel is
// constructed with one only because PanelRegistry's factory signature
// has one; it is ignored, deliberately and visibly.
public sealed class PublisherVerifyPanel : UserControl, IDisposable
{
    // P4 (bounded list) — a published site's committed key set is
    // unbounded. Show the first MaxKeysShown and say so; a truncated
    // list that does not announce itself reads as a complete one, which
    // on THIS panel would be a false claim about a key set.
    private const int MaxKeysShown = 500;

    private readonly long _handle;
    private readonly TextBox _originBox;
    private readonly TextBox _peerBox;
    private readonly CheckBox _bodiesBox;
    private readonly CheckBox _reconcileBox;
    private readonly TextBox _absentBox;
    private readonly Button _runButton;
    private readonly TextBlock _verdictLine;
    private readonly TextBlock _freshnessLine;
    private readonly TextBlock _rootLine;
    private readonly ItemsControl _stepList;
    private readonly ObservableCollection<StepRow> _steps = new();
    private readonly ListBox _keyList;
    private readonly ObservableCollection<string> _keys = new();

    private Bridge.TreeWakeCallback? _wakeCallback;
    private GCHandle _wakeCallbackHandle;
    private bool _disposed;

    public PublisherVerifyPanel(long peerHandle, IPanelHost? host = null)
    {
        _ = peerHandle; // see the class note: a verifying consumer is not a peer.

        var openReply = Bridge.TakeString(Bridge.VerifyOpen());
        _handle = ParseHandle(openReply);

        // Every control is built even on the failure path, and the
        // failure only replaces Content. The alternative — an early
        // return leaving half the fields null — is how a panel that
        // failed to open turns a later Refresh into a NullReference on
        // the UI thread, which in this runtime is a process death and
        // not an exception (MODEL-AVALONIA-RUNTIME, the X11/Skia
        // boundary).
        _originBox = new TextBox
        {
            Watermark = "https://origin.example  (or http://10.89.3.2:8099)",
            FontFamily = new FontFamily("monospace"),
            FontSize = 13,
        };
        _peerBox = new TextBox
        {
            Watermark = "expected peer-id (optional; cross-checked, never substituted)",
            FontFamily = new FontFamily("monospace"),
            FontSize = 12,
        };
        _absentBox = new TextBox
        {
            Watermark = "absent-key probe (optional)",
            FontFamily = new FontFamily("monospace"),
            FontSize = 12,
        };
        _bodiesBox = new CheckBox { Content = "fetch every leaf body", IsChecked = true, FontSize = 12 };
        _reconcileBox = new CheckBox { Content = "reconcile trie vs advertised leaf", IsChecked = true, FontSize = 12 };

        _runButton = new Button { Content = "Verify", FontSize = 13, Padding = new Thickness(16, 4) };
        _runButton.Click += (_, _) => StartRun();

        _verdictLine = new TextBlock
        {
            Text = "(not run)",
            FontWeight = FontWeight.SemiBold,
            FontSize = 14,
            TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(0, 8, 0, 0),
        };
        _freshnessLine = new TextBlock
        {
            Text = "",
            FontSize = 11,
            Opacity = 0.75,
            TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(0, 2, 0, 6),
        };
        _rootLine = new TextBlock
        {
            Text = "",
            FontFamily = new FontFamily("monospace"),
            FontSize = 11,
            Opacity = 0.8,
            TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(0, 0, 0, 8),
        };

        _stepList = new ItemsControl
        {
            ItemsSource = _steps,
            ItemTemplate = new FuncDataTemplate<StepRow>((row, _) => BuildStepView(row), supportsRecycling: false),
        };

        _keyList = new ListBox
        {
            ItemsSource = _keys,
            FontFamily = new FontFamily("monospace"),
            FontSize = 11,
            Background = Brushes.Transparent,
            BorderThickness = new Thickness(0),
            ItemTemplate = new FuncDataTemplate<string>((line, _) =>
                new SelectableTextBlock { Text = line, Opacity = 0.75, TextWrapping = TextWrapping.NoWrap },
                supportsRecycling: true),
        };

        var controls = new StackPanel { Spacing = 4, Margin = new Thickness(0, 0, 0, 4) };
        controls.Children.Add(new TextBlock
        {
            Text = "Publisher verification — the chain, not the pages",
            FontWeight = FontWeight.SemiBold,
            FontSize = 14,
            Opacity = 0.85,
        });
        controls.Children.Add(_originBox);
        controls.Children.Add(_peerBox);
        controls.Children.Add(_absentBox);
        var row = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 12 };
        row.Children.Add(_bodiesBox);
        row.Children.Add(_reconcileBox);
        row.Children.Add(_runButton);
        controls.Children.Add(row);
        controls.Children.Add(_verdictLine);
        controls.Children.Add(_freshnessLine);
        controls.Children.Add(_rootLine);

        var body = new StackPanel { Spacing = 6 };
        body.Children.Add(_stepList);
        body.Children.Add(new TextBlock
        {
            Text = "committed keys",
            FontSize = 11,
            Opacity = 0.6,
            Margin = new Thickness(0, 8, 0, 0),
        });
        body.Children.Add(_keyList);

        var dock = new DockPanel { LastChildFill = true, Margin = new Thickness(8) };
        DockPanel.SetDock(controls, Dock.Top);
        dock.Children.Add(controls);
        dock.Children.Add(new ScrollViewer { Content = body });
        Content = dock;

        if (_handle < 0)
        {
            Content = new SelectableTextBlock
            {
                Text = $"verify open failed: {openReply}",
                Foreground = Brushes.IndianRed,
                Margin = new Thickness(12),
                FontSize = 14,
            };
            return;
        }

        _wakeCallback = OnWakeFromGo;
        _wakeCallbackHandle = GCHandle.Alloc(_wakeCallback);
        var cbPtr = Marshal.GetFunctionPointerForDelegate(_wakeCallback);
        Bridge.TakeString(Bridge.VerifyRegisterWake(_handle, cbPtr));
        PanelLog.Write("verify", $"Mount h={_handle}");
    }

    // BuildStepView renders one link of the chain. The `proves` text is
    // part of the row, not a tooltip: a claim's scope that only appears
    // on hover is a claim's scope nobody reads.
    private static Control BuildStepView(StepRow row)
    {
        var stack = new StackPanel { Margin = new Thickness(0, 3, 0, 3) };
        var head = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 8 };
        head.Children.Add(new TextBlock
        {
            Text = row.Mark,
            Foreground = row.MarkBrush,
            FontFamily = new FontFamily("monospace"),
            FontWeight = FontWeight.Bold,
            FontSize = 12,
            Width = 56,
        });
        head.Children.Add(new TextBlock
        {
            Text = row.Name,
            FontWeight = FontWeight.SemiBold,
            FontSize = 12,
            Width = 150,
        });
        head.Children.Add(new SelectableTextBlock
        {
            Text = row.Detail,
            FontFamily = new FontFamily("monospace"),
            FontSize = 11,
            Opacity = 0.8,
            TextWrapping = TextWrapping.Wrap,
        });
        stack.Children.Add(head);
        if (!string.IsNullOrEmpty(row.Note))
        {
            stack.Children.Add(new TextBlock
            {
                Text = row.Note,
                FontSize = 11,
                Opacity = row.NoteIsError ? 0.95 : 0.6,
                Foreground = row.NoteIsError ? Brushes.IndianRed : null,
                TextWrapping = TextWrapping.Wrap,
                Margin = new Thickness(64, 1, 0, 0),
            });
        }
        return stack;
    }

    private void StartRun()
    {
        if (_disposed || _handle < 0) return;

        var cfg = JsonSerializer.Serialize(new VerifyConfig
        {
            Origin = _originBox.Text ?? "",
            PeerId = _peerBox.Text ?? "",
            Bodies = _bodiesBox.IsChecked == true,
            Reconcile = _reconcileBox.IsChecked == true,
            Absent = _absentBox.Text ?? "",
        });
        Bridge.TakeString(Bridge.VerifyConfigure(_handle, cfg));

        var reply = Bridge.TakeString(Bridge.VerifyStart(_handle));
        PanelLog.Write("verify", $"Start h={_handle} reply={reply}");
        _runButton.IsEnabled = false;
        // Mark in-flight BEFORE the bridge call. Without this the
        // completion transition is unobservable when the first Refresh
        // beats the goroutine into the operation — AP32.
        _lastRunning = true;
        _verdictLine.Text = "verifying…";
        _verdictLine.Foreground = null;
        _freshnessLine.Text = "";
        _rootLine.Text = "";
        _steps.Clear();
        _keys.Clear();
    }

    // OnWakeFromGo runs on a Go goroutine. Marshal to the UI thread
    // before touching a single control — the substrate model is explicit
    // that Avalonia's visual tree is UI-thread-affine and a cross-thread
    // mutation here is a crash, not an exception.
    private void OnWakeFromGo(long handle)
    {
        if (_disposed) return;
        Dispatcher.UIThread.Post(Refresh, DispatcherPriority.Background);
    }

    private void Refresh()
    {
        if (_disposed || _handle < 0) return;

        var json = Bridge.TakeString(Bridge.VerifyRender(_handle));
        VerifyEnvelope? env;
        try
        {
            env = JsonSerializer.Deserialize<VerifyEnvelope>(json, JsonOpts);
        }
        catch (Exception ex)
        {
            PanelLog.Write("verify", $"Render decode failed: {ex.Message}");
            _verdictLine.Text = "(render decode failed — see the panel log)";
            _runButton.IsEnabled = true;
            NoteRunFinished();
            return;
        }
        var rep = env?.Report;
        if (rep == null)
        {
            _verdictLine.Text = env?.Error ?? "(no report)";
            _runButton.IsEnabled = true;
            NoteRunFinished();
            return;
        }

        _runButton.IsEnabled = !rep.Running;
        if (!rep.Running) NoteRunFinished(); else _lastRunning = true;

        _steps.Clear();
        foreach (var s in rep.Steps ?? new List<Step>())
        {
            var (mark, brush) = s.Status switch
            {
                "ok" => ("  ok  ", Brushes.MediumSeaGreen),
                "failed" => (" FAIL ", (IBrush)Brushes.IndianRed),
                "skipped" => (" skip ", Brushes.Gray),
                _ => ("  ..  ", (IBrush)Brushes.Gray),
            };
            var note = !string.IsNullOrEmpty(s.Err) ? s.Err
                : (s.Status == "ok" ? s.Proves : "");
            _steps.Add(new StepRow(mark, brush, s.Name ?? "", s.Detail ?? "",
                note ?? "", !string.IsNullOrEmpty(s.Err)));
        }

        _keys.Clear();
        var keys = rep.Keys ?? new List<KeyRow>();
        var shown = Math.Min(keys.Count, MaxKeysShown);
        for (var i = 0; i < shown; i++)
        {
            var k = keys[i];
            var flag = !string.IsNullOrEmpty(k.Err) ? "FAIL"
                : k.Reconciled ? "ok ✓" : "ok  ";
            var tail = !string.IsNullOrEmpty(k.Err) ? k.Err
                : $"{Short(k.Hash)}  {k.Type}";
            _keys.Add($"{flag}  {k.Key,-46} {tail}");
        }
        if (keys.Count > shown)
        {
            _keys.Add($"… {keys.Count - shown} more committed keys not shown (panel cap {MaxKeysShown})");
        }

        if (!string.IsNullOrEmpty(rep.RootHash))
        {
            var layoutNote = rep.Discovered
                ? "layout discovered from the origin's transport-profile"
                : "layout PINNED by you — a wrong pin and a withholding origin look identical";
            _rootLine.Text =
                $"root {rep.RootHash}\n" +
                $"prefix {rep.Prefix}  →  {rep.AbsolutePrefix}   (§3.3 absolute form)\n" +
                $"seq {rep.Seq} · {rep.Nodes} CHAMP nodes · {rep.KeysTotal} committed keys · {layoutNote}";
        }

        if (rep.Running)
        {
            _verdictLine.Text = "verifying…";
            _verdictLine.Foreground = null;
        }
        else if (rep.Incomplete)
        {
            _verdictLine.Foreground = Brushes.IndianRed;
            _verdictLine.Text = "INCOMPLETE WALK — this origin does not serve the closure its own " +
                "signed root commits to. That is a statement about the publisher, not about " +
                "reachability: a per-leaf fetch of any single page would have succeeded.";
        }
        else if (!string.IsNullOrEmpty(rep.Err))
        {
            _verdictLine.Foreground = Brushes.IndianRed;
            _verdictLine.Text = "FAILED — " + rep.Err;
        }
        else if (rep.Verified)
        {
            _verdictLine.Foreground = Brushes.MediumSeaGreen;
            _verdictLine.Text = $"chain held · {rep.KeysOk} keys verified · {rep.Duration}";
        }
        else if (rep.HasRun)
        {
            _verdictLine.Foreground = Brushes.IndianRed;
            _verdictLine.Text = $"{rep.KeysFailed} of {rep.KeysTotal} committed keys failed verification";
        }

        // The freshness sentence is never dropped on a green result. It
        // is the difference between "verified" and the only claim this
        // corridor can actually support.
        _freshnessLine.Text = rep.FreshnessNote ?? "";
    }

    // ---- test hooks (Tier 3) ----------------------------------------
    //
    // A verification is asynchronous by construction (the bridge moves
    // the network work off the UI thread), so a test needs a way to
    // start one and settle. RunForTests polls Render rather than
    // relying on the wake, because a headless test has no real
    // dispatcher loop pumping Go callbacks.

    public long HandleForTests => _handle;
    public string VerdictTextForTests => _verdictLine?.Text ?? "";
    public int StepCountForTests => _steps.Count;
    public IReadOnlyList<string> StepMarksForTests
    {
        get
        {
            var marks = new List<string>();
            foreach (var s in _steps) marks.Add(s.Mark.Trim());
            return marks;
        }
    }

    // _lastRunning / _runsCompleted are the completion signal. The
    // button's IsEnabled is NOT one: it is true before a run starts, so
    // a test that settles on it can return in the window between the
    // bridge call returning and the goroutine entering the operation,
    // then assert against an empty report. That is AP32, and it is what
    // made Origin_Serving_A_Root_..._Incomplete_Walk fail intermittently
    // ("Assert.Contains() Failure: Item not found in collection" — the
    // step list was empty because nothing had run yet).
    private bool _lastRunning;
    private long _runsCompleted;

    private void NoteRunFinished()
    {
        if (!_lastRunning) return;
        _lastRunning = false;
        _runsCompleted++;
    }

    public long RunsCompletedForTests => _runsCompleted;

    public void RunForTests(string origin, int timeoutMs = 8000)
    {
        _originBox.Text = origin;
        var before = _runsCompleted;
        StartRun();
        var deadline = DateTime.UtcNow.AddMilliseconds(timeoutMs);
        while (DateTime.UtcNow < deadline)
        {
            Refresh();
            // Wait for the monotonic completion counter to advance, not
            // for a derived UI property to look right.
            if (_runsCompleted > before) return;
            System.Threading.Thread.Sleep(15);
        }
        Refresh();
    }

    private static string Short(string? h)
    {
        if (string.IsNullOrEmpty(h)) return "";
        return h.Length > 26 ? h.Substring(0, 26) + "…" : h;
    }

    private static long ParseHandle(string envelope)
    {
        try
        {
            using var doc = JsonDocument.Parse(envelope);
            var root = doc.RootElement;
            if (root.TryGetProperty("ok", out var ok) && ok.ValueKind == JsonValueKind.True
                && root.TryGetProperty("handle", out var h))
            {
                return h.GetInt64();
            }
        }
        catch (JsonException)
        {
            // fall through
        }
        return -1;
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        if (_handle >= 0) Bridge.TakeString(Bridge.VerifyClose(_handle));
        if (_wakeCallbackHandle.IsAllocated) _wakeCallbackHandle.Free();
        _wakeCallback = null;
        PanelLog.Write("verify", $"Dispose h={_handle}");
    }

    private sealed record StepRow(string Mark, IBrush MarkBrush, string Name, string Detail,
        string Note, bool NoteIsError);

    private static readonly JsonSerializerOptions JsonOpts = new()
    {
        PropertyNameCaseInsensitive = true,
    };

    private sealed class VerifyConfig
    {
        [JsonPropertyName("origin")] public string Origin { get; set; } = "";
        [JsonPropertyName("peer_id")] public string PeerId { get; set; } = "";
        [JsonPropertyName("bodies")] public bool Bodies { get; set; }
        [JsonPropertyName("reconcile")] public bool Reconcile { get; set; }
        [JsonPropertyName("absent")] public string Absent { get; set; } = "";
    }

    private sealed class VerifyEnvelope
    {
        [JsonPropertyName("ok")] public bool Ok { get; set; }
        [JsonPropertyName("error")] public string? Error { get; set; }
        [JsonPropertyName("report")] public Report? Report { get; set; }
    }

    private sealed class Report
    {
        [JsonPropertyName("Origin")] public string? Origin { get; set; }
        [JsonPropertyName("PeerID")] public string? PeerId { get; set; }
        [JsonPropertyName("Discovered")] public bool Discovered { get; set; }
        [JsonPropertyName("Steps")] public List<Step>? Steps { get; set; }
        [JsonPropertyName("Keys")] public List<KeyRow>? Keys { get; set; }
        [JsonPropertyName("RootHash")] public string? RootHash { get; set; }
        [JsonPropertyName("Prefix")] public string? Prefix { get; set; }
        [JsonPropertyName("AbsolutePrefix")] public string? AbsolutePrefix { get; set; }
        [JsonPropertyName("Seq")] public ulong Seq { get; set; }
        [JsonPropertyName("Nodes")] public int Nodes { get; set; }
        [JsonPropertyName("KeysTotal")] public int KeysTotal { get; set; }
        [JsonPropertyName("KeysOK")] public int KeysOk { get; set; }
        [JsonPropertyName("KeysFailed")] public int KeysFailed { get; set; }
        [JsonPropertyName("Verified")] public bool Verified { get; set; }
        [JsonPropertyName("Incomplete")] public bool Incomplete { get; set; }
        [JsonPropertyName("Err")] public string? Err { get; set; }
        [JsonPropertyName("Running")] public bool Running { get; set; }
        [JsonPropertyName("HasRun")] public bool HasRun { get; set; }
        [JsonPropertyName("Duration")] public string? Duration { get; set; }
        [JsonPropertyName("freshness_note")] public string? FreshnessNote { get; set; }
    }

    private sealed class Step
    {
        [JsonPropertyName("Name")] public string? Name { get; set; }
        [JsonPropertyName("Status")] public string? Status { get; set; }
        [JsonPropertyName("Detail")] public string? Detail { get; set; }
        [JsonPropertyName("Proves")] public string? Proves { get; set; }
        [JsonPropertyName("Err")] public string? Err { get; set; }
    }

    private sealed class KeyRow
    {
        [JsonPropertyName("Key")] public string? Key { get; set; }
        [JsonPropertyName("Hash")] public string? Hash { get; set; }
        [JsonPropertyName("Type")] public string? Type { get; set; }
        [JsonPropertyName("Bytes")] public int Bytes { get; set; }
        [JsonPropertyName("Reconciled")] public bool Reconciled { get; set; }
        [JsonPropertyName("Err")] public string? Err { get; set; }
    }
}
