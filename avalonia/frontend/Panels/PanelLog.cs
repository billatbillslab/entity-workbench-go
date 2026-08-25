using System;
using System.IO;

namespace EntityAvalonia.Panels;

// PanelLog: minimal stderr-buffered breadcrumb log. The bug class
// we've been chasing (stack-overflow-style crash with no managed
// dump) prevents .NET from writing any post-mortem trace. The only
// thing we get is the last line written to stderr BEFORE the crash.
// Pre-allocating + flushing every line means the last successful
// operation is captured.
//
// Enable by setting the WB_PANEL_LOG environment variable to any
// non-empty value. Off by default (zero overhead) so the test
// suite isn't slowed.
public static class PanelLog
{
    private static readonly bool _enabled;
    private static readonly TextWriter _out;

    static PanelLog()
    {
        // WB_CRASH_TRACE implies panel logging: a first-chance trace that
        // records nowhere is not a trace.
        var v = Environment.GetEnvironmentVariable("WB_PANEL_LOG");
        var t = Environment.GetEnvironmentVariable("WB_CRASH_TRACE");
        _enabled = !string.IsNullOrEmpty(v) || !string.IsNullOrEmpty(t);
        _out = Console.Error;
    }

    public static bool Enabled => _enabled;

    public static void Write(string tag, string message)
    {
        // RECORD unconditionally, PRINT on opt-in. The ring is in-memory
        // and costs a string + an enqueue; the crash decides when we
        // needed it, and by then WB_PANEL_LOG can no longer be set.
        // (2026-08-21: two SIGSEGVs, and the only reason we had any
        // breadcrumbs at all was that the operator happened to launch
        // via `make up`, which exports WB_PANEL_LOG. `make gui-run` does
        // not.)
        CrashDiagnostics.Breadcrumb(tag, message);

        if (!_enabled) return;
        var ts = DateTime.UtcNow.ToString("HH:mm:ss.fff");
        _out.WriteLine($"[panel {ts}] {tag}: {message}");
        _out.Flush();
    }
}
