package entitysdk

import (
	"fmt"
	"sync"
	"time"
)

// LogLevel controls which events are recorded.
type LogLevel int

const (
	LogInfo    LogLevel = iota // application events (default)
	LogVerbose                 // + executor operations (tree get/list/put)
	LogDebug                   // + entity data summaries
)

// EventLog is a thread-safe append-only log of application events.
// Panels bind to it to display real-time event streams.
type EventLog struct {
	mu        sync.Mutex
	entries   []LogEntry
	maxSize   int
	level     LogLevel
	seq       uint64 // monotonic per-append sequence; populates LogEntry.Seq
	listeners []*appendListener
}

// appendListener wraps an OnAppend callback in a uniquely-identifiable
// container so OnAppend's returned cancel func can find and remove
// exactly its own listener (pointer identity) without depending on
// callback-equality, which Go can't do reliably.
type appendListener struct {
	cb func()
}

// LogEntry is a single timestamped event.
type LogEntry struct {
	Seq     uint64 // monotonic sequence number
	Time    time.Time
	Level   LogLevel
	Message string
}

// NewEventLog creates an event log with a maximum entry count.
// Defaults to LogDebug collection level (collect everything).
func NewEventLog(maxSize int) *EventLog {
	return &EventLog{maxSize: maxSize, level: LogDebug}
}

// Append adds an info-level entry to the log.
func (el *EventLog) Append(msg string) {
	el.appendAt(LogInfo, msg)
}

// Appendf adds a formatted info-level entry.
func (el *EventLog) Appendf(format string, args ...interface{}) {
	el.appendAt(LogInfo, fmt.Sprintf(format, args...))
}

// Verbose adds a verbose-level entry (only recorded when level >= LogVerbose).
func (el *EventLog) Verbose(msg string) {
	el.appendAt(LogVerbose, msg)
}

// Verbosef adds a formatted verbose-level entry.
func (el *EventLog) Verbosef(format string, args ...interface{}) {
	el.appendAt(LogVerbose, fmt.Sprintf(format, args...))
}

// Debug adds a debug-level entry (only recorded when level >= LogDebug).
func (el *EventLog) Debug(msg string) {
	el.appendAt(LogDebug, msg)
}

// Debugf adds a formatted debug-level entry.
func (el *EventLog) Debugf(format string, args ...interface{}) {
	el.appendAt(LogDebug, fmt.Sprintf(format, args...))
}

func (el *EventLog) appendAt(level LogLevel, msg string) {
	el.mu.Lock()
	if level > el.level {
		el.mu.Unlock()
		return
	}
	el.seq++
	el.entries = append(el.entries, LogEntry{
		Seq:     el.seq,
		Time:    time.Now(),
		Level:   level,
		Message: msg,
	})
	if len(el.entries) > el.maxSize {
		el.entries = el.entries[len(el.entries)-el.maxSize:]
	}
	// Snapshot listeners under the lock so subscribers added/removed
	// concurrently don't race; fire them outside the lock so a slow
	// listener can't back-pressure the writer.
	listeners := make([]*appendListener, len(el.listeners))
	copy(listeners, el.listeners)
	el.mu.Unlock()
	for _, l := range listeners {
		l.cb()
	}
}

// OnAppend registers a callback that fires (no payload) after every
// successful entry append. Returns a cancel func; calling it removes
// the listener. The callback runs on the appending goroutine — keep
// it cheap (push to a channel, then return).
func (el *EventLog) OnAppend(cb func()) func() {
	l := &appendListener{cb: cb}
	el.mu.Lock()
	el.listeners = append(el.listeners, l)
	el.mu.Unlock()
	return func() {
		el.mu.Lock()
		defer el.mu.Unlock()
		for i, existing := range el.listeners {
			if existing == l {
				el.listeners = append(el.listeners[:i], el.listeners[i+1:]...)
				return
			}
		}
	}
}

// SetLevel changes the verbosity level.
func (el *EventLog) SetLevel(level LogLevel) {
	el.mu.Lock()
	el.level = level
	el.mu.Unlock()
}

// Level returns the current verbosity level.
func (el *EventLog) Level() LogLevel {
	el.mu.Lock()
	defer el.mu.Unlock()
	return el.level
}

// LevelName returns a display name for the current level.
func (el *EventLog) LevelName() string {
	switch el.Level() {
	case LogInfo:
		return "info"
	case LogVerbose:
		return "verbose"
	case LogDebug:
		return "debug"
	default:
		return "unknown"
	}
}

// Entries returns a snapshot of all log entries.
func (el *EventLog) Entries() []LogEntry {
	el.mu.Lock()
	defer el.mu.Unlock()
	out := make([]LogEntry, len(el.entries))
	copy(out, el.entries)
	return out
}

// Len returns the current entry count.
func (el *EventLog) Len() int {
	el.mu.Lock()
	defer el.mu.Unlock()
	return len(el.entries)
}
