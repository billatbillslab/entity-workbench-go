using System;
using System.Collections.Generic;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using Avalonia.Controls;
using Avalonia.Input;
using Avalonia.Interactivity;
using Avalonia.Threading;

namespace EntityAvalonia;

// CrashDiagnostics — the always-on forensic surface for a crash class
// that leaves nothing behind.
//
// WHY THIS EXISTS (measured, 2026-08-21). Two `entity-avalonia` SIGSEGVs
// on the same day produced, between them: no managed minidump (createdump
// did not fire even though run-with-dump.sh exports
// DOTNET_DbgEnableMiniDump=1), no managed stack (the DAC refuses to load
// against a systemd-coredump ELF core — `Failed to load data access
// module, 0x80004002`), and no breadcrumb naming the last user action
// (`make crash` decodes libbridge.so Go symbols, and neither faulting
// thread had a single libbridge frame). The `si_code` on both dumps is
// 128 / SI_KERNEL with `si_addr = 0`, i.e. the signal was RE-RAISED —
// so even the register context is not the fault site. Four independent
// forensic channels, four blanks.
//
// A crash we cannot characterize is a crash we cannot fix, and the
// operator cannot be the instrument (they do not have time to sit and
// manually reproduce). So the app records its own last moments:
//
//   1. A ring of the last N breadcrumbs — ALWAYS kept in memory, even
//      when WB_PANEL_LOG is unset. PanelLog feeds it. Printing is opt-in;
//      RECORDING is not, because the crash decides when we needed it.
//   2. Global input breadcrumbs on the TopLevel, TUNNELING so they run
//      BEFORE the target's own handler. This is the specific blind spot
//      that cost us the 13:16 dump: the user clicked a link, the fault
//      landed before `SiteViewPanel.NavigateTo`'s first log line, and
//      the run log therefore ended eight seconds before the crash with
//      no hint that a click had ever happened.
//   3. Managed exception handlers (AppDomain / Dispatcher / Task) that
//      dump the exception, its stack, AND the breadcrumb ring.
//   4. A durable file under ~/.entity/crash/ — stderr is only captured
//      by `make up`, and `make gui-run` / `host-run` (the documented
//      fast loop) discards it. `make extract` wipes dist-native, so the
//      log cannot live beside the binary.
//
// LIMIT, stated plainly: a hard SIGSEGV is NOT a managed exception and
// will not run any handler here. What this class guarantees for that
// case is the breadcrumb ring on disk up to the last flushed line —
// which is exactly what was missing. It converts "no information" into
// "the last action before it died", and it converts every *managed*
// fault (the NullReferenceException class, which is the leading
// hypothesis) into a full symbolized stack with zero operator effort.
public static class CrashDiagnostics
{
    private const int RingCapacity = 96;

    private static readonly object _gate = new();
    private static readonly Queue<string> _ring = new(RingCapacity);
    private static bool _installed;
    private static string? _crashLogPath;
    private static bool _fatalWritten;

    // Trace mode logs first-chance exceptions too. Off by default: a
    // healthy Avalonia session throws and catches a fair number of them
    // internally, so this is a debugging instrument, not a default.
    private static bool _trace;

    public static string? CrashLogPath => _crashLogPath;

    // Install wires the process-wide handlers. Idempotent; safe to call
    // before Avalonia starts (it does not touch the dispatcher).
    public static void Install()
    {
        lock (_gate)
        {
            if (_installed) return;
            _installed = true;
        }

        _trace = !string.IsNullOrEmpty(Environment.GetEnvironmentVariable("WB_CRASH_TRACE"));
        _crashLogPath = ResolveCrashLogPath();

        AppDomain.CurrentDomain.UnhandledException += (_, e) =>
            WriteFatal("AppDomain.UnhandledException" + (e.IsTerminating ? " (terminating)" : ""),
                e.ExceptionObject as Exception, e.ExceptionObject);

        System.Threading.Tasks.TaskScheduler.UnobservedTaskException += (_, e) =>
        {
            WriteFatal("TaskScheduler.UnobservedTaskException", e.Exception, null);
            // Observing it keeps a background fault from escalating into
            // a process kill on a runtime configured to do so. We have
            // already recorded it; escalating adds nothing.
            e.SetObserved();
        };

        if (_trace)
        {
            AppDomain.CurrentDomain.FirstChanceException += (_, e) =>
            {
                // First-chance is noisy AND re-entrant: writing to the
                // ring can itself throw. Keep it to one cheap line, and
                // print it — a managed exception thrown just before a
                // hard fault is the strongest lead there is, and it has
                // to survive the signal.
                Panels.PanelLog.Write("first-chance",
                    e.Exception.GetType().Name + ": " + Truncate(e.Exception.Message, 160));
            };
        }

        Breadcrumb("crash-diag", "installed" + (_trace ? " (trace on)" : "") +
            (_crashLogPath != null ? " log=" + _crashLogPath : " log=<none>"));

        // Report, then optionally replace, this thread's alternate
        // signal stack. Main() runs on the UI thread, so installing here
        // protects the thread the crash lands on.
        Panels.PanelLog.Write("altstack", "at startup: " + ReportAltStack());

        // DEFAULT ON. The PAL gives the UI thread a 16 KB alternate
        // signal stack (measured), and 16 KB is not enough for the
        // handler chain this process actually runs: the click fuzz
        // crashed 5 seeds out of 5 on the stock size and 0 out of 5 at
        // 1 MB, same seeds, same binary.
        //
        // The cost is a 1 MB PRIVATE|ANONYMOUS mapping that commits
        // lazily — only the pages a signal handler actually touches are
        // ever backed — against a fatal, unrecoverable process kill.
        // That trade is not close.
        //
        // WB_ALTSTACK_BYTES overrides the size; 0 disables entirely and
        // restores the stock behaviour, which is what the A/B control
        // arm uses. Keep that escape hatch: it is the only way to
        // re-measure the bug once this is in.
        long want = 1L << 20;
        var overrideBytes = Environment.GetEnvironmentVariable("WB_ALTSTACK_BYTES");
        if (!string.IsNullOrEmpty(overrideBytes) && long.TryParse(overrideBytes, out var n)) want = n;
        string outcome;
        if (want > 0)
        {
            var installed = EnlargeAltStack(want);
            Panels.PanelLog.Write("altstack", $"enlarge -> {installed}");
            Panels.PanelLog.Write("altstack", "after: " + ReportAltStack());
            outcome = installed;
        }
        else
        {
            outcome = "DISABLED by WB_ALTSTACK_BYTES=0 (stock 16 KB — the crashing config)";
            Panels.PanelLog.Write("altstack", "enlargement " + outcome);
        }

        // Announced unconditionally, for the same reason the render mode
        // is (Program.BuildAvaloniaApp): it is the first thing a crash
        // report needs and the last thing a reporter thinks to include.
        // WB_PANEL_LOG is off on the documented fast loop (`make gui-run`),
        // so a PanelLog line alone would be invisible exactly when it
        // matters.
        try
        {
            Console.Error.WriteLine("entity-avalonia: alt signal stack = " + outcome);
        }
        catch { }
    }

    // InstallDispatcher hooks Avalonia's UI-thread exception event. It is
    // separate from Install() because the dispatcher does not exist until
    // the framework is up.
    public static void InstallDispatcher()
    {
        try
        {
            Dispatcher.UIThread.UnhandledException += (_, e) =>
            {
                WriteFatal("Dispatcher.UnhandledException", e.Exception, null);
                // Deliberately NOT setting e.Handled. Swallowing a UI
                // fault leaves the app running in an undefined state and
                // turns one diagnosable crash into a stream of downstream
                // mysteries. We record, then let it take its course.
            };
        }
        catch (Exception ex)
        {
            Breadcrumb("crash-diag", "dispatcher hook failed: " + ex.Message);
        }
    }

    // AttachInput records every pointer press and key down on a TopLevel
    // BEFORE the target control's own handler sees it (Tunnel). This is
    // the "what did the user just do" channel.
    public static void AttachInput(TopLevel top)
    {
        try
        {
            top.AddHandler(InputElement.PointerPressedEvent, OnPointerPressed,
                RoutingStrategies.Tunnel, handledEventsToo: true);
            top.AddHandler(InputElement.KeyDownEvent, OnKeyDown,
                RoutingStrategies.Tunnel, handledEventsToo: true);
            Breadcrumb("input", "breadcrumbs attached to " + top.GetType().Name);
        }
        catch (Exception ex)
        {
            Breadcrumb("crash-diag", "input hook failed: " + ex.Message);
        }
    }

    // Input breadcrumbs go through PanelLog, not straight to the ring.
    //
    // The ring alone is not enough and the first harness run proved it: a
    // hard SIGSEGV never runs WriteFatal, so an in-memory ring dies with
    // the process. PanelLog flushes each line to stderr, which is the
    // only channel that survives a signal — and the run log is exactly
    // where "what did the user just do" needs to appear.
    private static void OnPointerPressed(object? sender, PointerPressedEventArgs e)
    {
        try
        {
            var pt = e.GetCurrentPoint(null).Position;
            Panels.PanelLog.Write("input", $"press @{pt.X:F0},{pt.Y:F0} on {Describe(e.Source)}");
        }
        catch
        {
            // A breadcrumb must never be the thing that breaks the app.
            Panels.PanelLog.Write("input", "press <describe failed>");
        }
    }

    private static void OnKeyDown(object? sender, KeyEventArgs e)
    {
        try
        {
            Panels.PanelLog.Write("input", $"key {e.Key} mods={e.KeyModifiers} on {Describe(e.Source)}");
        }
        catch
        {
            Panels.PanelLog.Write("input", "key <describe failed>");
        }
    }

    // Describe names a control well enough to find it in source: type,
    // x:Name if set, and the text of a Button/TextBlock (truncated).
    private static string Describe(object? source)
    {
        if (source == null) return "<null>";
        var t = source.GetType().Name;
        if (source is not Control c) return t;

        var sb = new StringBuilder(t);
        if (!string.IsNullOrEmpty(c.Name)) sb.Append('#').Append(c.Name);

        string? text = c switch
        {
            Button b => b.Content as string,
            TextBlock tb => tb.Text,
            TextBox box => box.Text,
            ContentControl cc => cc.Content as string,
            _ => null,
        };
        if (!string.IsNullOrEmpty(text)) sb.Append(" \"").Append(Truncate(text!, 48)).Append('"');
        return sb.ToString();
    }

    // Breadcrumb records into the ring. Cheap, allocation-light, never
    // throws. PanelLog also calls this, so panel breadcrumbs are captured
    // whether or not WB_PANEL_LOG is printing them.
    public static void Breadcrumb(string tag, string message)
    {
        try
        {
            var line = $"[{DateTime.UtcNow:HH:mm:ss.fff}] {tag}: {message}";
            lock (_gate)
            {
                if (_ring.Count >= RingCapacity) _ring.Dequeue();
                _ring.Enqueue(line);
            }
        }
        catch
        {
            // Intentionally empty — see the comment above.
        }
    }

    // Snapshot returns the current ring, oldest first.
    public static IReadOnlyList<string> Snapshot()
    {
        lock (_gate)
        {
            return new List<string>(_ring);
        }
    }

    private static void WriteFatal(string channel, Exception? ex, object? raw)
    {
        var sb = new StringBuilder();
        sb.AppendLine();
        sb.AppendLine("========== entity-avalonia FATAL ==========");
        sb.Append("when:    ").AppendLine(DateTime.UtcNow.ToString("yyyy-MM-dd HH:mm:ss.fff") + "Z");
        sb.Append("channel: ").AppendLine(channel);
        sb.Append("pid:     ").AppendLine(Environment.ProcessId.ToString());
        sb.Append("thread:  ").AppendLine(Environment.CurrentManagedThreadId +
            (Dispatcher.UIThread.CheckAccess() ? " (UI thread)" : " (background)"));
        sb.Append("render:  ").AppendLine(
            string.IsNullOrEmpty(Environment.GetEnvironmentVariable("WB_GPU_RENDER"))
                ? "software Skia" : "GPU (WB_GPU_RENDER set)");

        if (ex != null)
        {
            sb.AppendLine("---- exception ----");
            sb.AppendLine(ex.ToString());
        }
        else if (raw != null)
        {
            sb.AppendLine("---- non-exception throw ----");
            sb.AppendLine(raw.ToString());
        }

        sb.AppendLine("---- breadcrumbs (oldest first) ----");
        foreach (var line in Snapshot()) sb.AppendLine(line);
        sb.AppendLine("========== end ==========");

        var text = sb.ToString();

        // stderr first: it is the channel a smoke harness or `make up`
        // is already capturing.
        try
        {
            Console.Error.Write(text);
            Console.Error.Flush();
        }
        catch { /* stderr can be gone during shutdown */ }

        // Then the durable file. Guard against a fault storm writing
        // megabytes: the first fatal is the one that matters.
        try
        {
            if (_crashLogPath != null)
            {
                bool first;
                lock (_gate) { first = !_fatalWritten; _fatalWritten = true; }
                if (first || _trace)
                {
                    File.AppendAllText(_crashLogPath, text);
                }
            }
        }
        catch { /* a diagnostic that throws is worse than no diagnostic */ }
    }

    // ---- alternate signal stack -------------------------------------
    //
    // Measured 2026-08-21, under gdb, on a reproducible crash:
    //
    //     Thread 1 received signal SIG34 (the runtime's thread-suspend
    //     injection), then immediately
    //     Thread 1 received signal SIGSEGV, si_code=2 (SEGV_ACCERR),
    //     si_addr = rsp-8, rip on a `call` instruction,
    //     and rsp inside a PROT_NONE page.
    //
    // A faulting `call` whose pushed return address lands in a guard
    // page is a stack overflow, and the mapping rsp sat in was NOT the
    // managed stack — it was the small anonymous region the PAL sets up
    // as the ALTERNATE SIGNAL STACK. So the overflow is signal-handler
    // stack exhaustion, which is why the runtime never printed
    // "Stack overflow." and why createdump never fired: by the time it
    // faults, the process has no stack left to report on.
    //
    // ReportAltStack prints what the thread actually has, so the theory
    // is checkable rather than inferred from a mapping table.
    // EnlargeAltStack replaces it with a larger one.
    [StructLayout(LayoutKind.Sequential)]
    private struct StackT
    {
        public IntPtr ss_sp;
        public int ss_flags;
        public IntPtr ss_size;
    }

    [DllImport("libc", SetLastError = true)]
    private static extern int sigaltstack(IntPtr newStack, IntPtr oldStack);

    [DllImport("libc", SetLastError = true)]
    private static extern IntPtr mmap(IntPtr addr, IntPtr length, int prot, int flags, int fd, IntPtr offset);

    private const int PROT_READ = 1, PROT_WRITE = 2;
    private const int MAP_PRIVATE = 0x02, MAP_ANONYMOUS = 0x20, MAP_STACK = 0x20000;

    public static string ReportAltStack()
    {
        try
        {
            var buf = Marshal.AllocHGlobal(Marshal.SizeOf<StackT>());
            try
            {
                if (sigaltstack(IntPtr.Zero, buf) != 0)
                    return "sigaltstack query failed errno=" + Marshal.GetLastWin32Error();
                var st = Marshal.PtrToStructure<StackT>(buf);
                return $"sp=0x{st.ss_sp.ToInt64():x} size={st.ss_size.ToInt64()} flags={st.ss_flags}";
            }
            finally { Marshal.FreeHGlobal(buf); }
        }
        catch (Exception ex)
        {
            return "sigaltstack query threw: " + ex.Message;
        }
    }

    // EnlargeAltStack installs a bigger alternate signal stack for the
    // CURRENT thread. Must be called on the UI thread to protect it.
    // Returns a human-readable outcome; never throws.
    public static string EnlargeAltStack(long bytes)
    {
        try
        {
            var len = new IntPtr(bytes);
            var mem = mmap(IntPtr.Zero, len, PROT_READ | PROT_WRITE,
                MAP_PRIVATE | MAP_ANONYMOUS | MAP_STACK, -1, IntPtr.Zero);
            if (mem == IntPtr.Zero || mem.ToInt64() == -1)
                return "mmap failed errno=" + Marshal.GetLastWin32Error();

            var buf = Marshal.AllocHGlobal(Marshal.SizeOf<StackT>());
            try
            {
                Marshal.StructureToPtr(new StackT { ss_sp = mem, ss_flags = 0, ss_size = len }, buf, false);
                if (sigaltstack(buf, IntPtr.Zero) != 0)
                    return "sigaltstack install failed errno=" + Marshal.GetLastWin32Error();
                // Deliberately leaked: it must outlive every signal the
                // process will ever take.
                return $"installed {bytes} bytes at 0x{mem.ToInt64():x}";
            }
            finally { Marshal.FreeHGlobal(buf); }
        }
        catch (Exception ex)
        {
            return "enlarge threw: " + ex.Message;
        }
    }

    private static string ResolveCrashLogPath()
    {
        try
        {
            var dir = Environment.GetEnvironmentVariable("WB_CRASH_DIR");
            if (string.IsNullOrEmpty(dir))
            {
                var home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
                if (string.IsNullOrEmpty(home)) return null!;
                dir = Path.Combine(home, ".entity", "crash");
            }
            Directory.CreateDirectory(dir);
            return Path.Combine(dir, $"entity-avalonia-{Environment.ProcessId}.log");
        }
        catch
        {
            return null!;
        }
    }

    private static string Truncate(string s, int max) =>
        s.Length <= max ? s : s.Substring(0, max) + "…";
}
