package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	kservice "github.com/kardianos/service"
)

// The OS system log (Windows Event Log when running as a Windows service,
// syslog/journald via kardianos's syslog backend on Linux) is the sink for
// everything bento-ads itself logs in service mode before Bento's own
// logger exists, plus -- via osSlogHandler below -- everything Bento's
// logger emits once it does exist. kardianos's Logger interface only has
// Error/Warning/Info (there is no Debug), which is reflected in both types
// below.

// osErrWriter adapts a kardianos Logger to an io.Writer, for use with
// service.CLIOptSetErrWriter: every failure Bento's CLI prints before its
// own logger is constructed (a bad config path, a YAML parse error, etc)
// arrives here as one or more Write calls instead of going to stderr, which
// a Windows service has nothing attached to.
//
// Write logs each line of p at ERROR, synchronously (it does not return
// until the OS logger call has returned), matching CLIOptSetErrWriter's
// synchronous-before-os.Exit contract. A write with no trailing newline is
// still logged as a (final) line.
type osErrWriter struct {
	logger kservice.Logger
}

func (w *osErrWriter) Write(p []byte) (int, error) {
	lines := strings.Split(string(p), "\n")
	// A write ending in "\n" splits into a trailing "" we don't want to log
	// as an extra empty ERROR entry.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		if line == "" {
			continue
		}
		// kservice.Logger.Error's own error (e.g. the Event Log is
		// unreachable) has nowhere else to go; dropping it is the best we
		// can do here without a second fallback writer.
		_ = w.logger.Error(line)
	}
	return len(p), nil
}

// osSlogHandler is an slog.Handler that renders records onto a kservice
// Logger, for use with service.CLIOptAddTeeLogger. It only forwards tee
// records when the config has no file logger (logger.file already holds
// them), filtered by logger.level; records arriving before that's decided
// are buffered and flushed/discarded once it is, except ERROR+, which goes
// out immediately since Bento can os.Exit before the decision is made.
type osSlogHandler struct {
	logger kservice.Logger
	level  *atomic.Int64 // slog.Level, stored as int64
	attrs  []slog.Attr
	groups []string

	// state is shared (by pointer) across every handler returned by
	// WithAttrs/WithGroup, the same way level is: slog.Logger.With(...)
	// clones the handler, and the buffer/decision must be the same one
	// underneath every clone.
	state *teeState
}

// maxBufferedRecords bounds how many pre-decision tee records osSlogHandler
// holds onto; past this, the oldest is dropped (with a single logged note)
// rather than growing without bound while waiting for the file-logger
// decision.
const maxBufferedRecords = 1000

// bufferedRecord is a tee record captured before decideFileLogger has run,
// already rendered to its final line (Handle does that work once; replaying
// just re-dispatches it to the right kservice.Logger method).
type bufferedRecord struct {
	level slog.Level
	line  string
}

// teeState is osSlogHandler's mutable, shared state: whether the
// file-logger decision has been made yet, what it was, and the records
// buffered while waiting for it.
type teeState struct {
	mu           sync.Mutex
	decided      bool
	forward      bool // meaningful only once decided: true if there is no file logger
	buf          []bufferedRecord
	droppedNoted bool
}

// newOSSlogHandler builds an osSlogHandler defaulting to slog.LevelInfo
// until setLevel is called (normally from a CLIOptOnConfigParse hook, once
// logger.level has actually been read from the config).
func newOSSlogHandler(logger kservice.Logger) *osSlogHandler {
	h := &osSlogHandler{logger: logger, level: &atomic.Int64{}, state: &teeState{}}
	h.level.Store(int64(slog.LevelInfo))
	return h
}

// setLevel updates the level threshold used by Enabled. Safe to call
// concurrently with logging.
func (h *osSlogHandler) setLevel(l slog.Level) {
	h.level.Store(int64(l))
}

func (h *osSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.Level(h.level.Load())
}

func (h *osSlogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		writeAttr(&b, h.groups, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, h.groups, a)
		return true
	})
	line := b.String()

	st := h.state
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.decided {
		if !st.forward {
			// A file logger already has this record; the OS log doesn't
			// need it too.
			return nil
		}
		return h.emit(r.Level, line)
	}

	if r.Level >= slog.LevelError {
		// Can't wait for the decision: Bento may os.Exit before
		// decideFileLogger ever runs (e.g. a strict-lint failure), which
		// would lose this from the buffer entirely. Emitted now, so it's
		// deliberately not buffered below (no double emission on flush).
		return h.emit(r.Level, line)
	}

	// Not decided yet: hold onto it until decideFileLogger says whether to
	// forward or discard it.
	if len(st.buf) >= maxBufferedRecords {
		if !st.droppedNoted {
			st.droppedNoted = true
			_ = h.logger.Warning("bento-ads: pre-decision log buffer full; oldest buffered Bento log records are being dropped")
		}
		st.buf = st.buf[1:]
	}
	st.buf = append(st.buf, bufferedRecord{level: r.Level, line: line})
	return nil
}

// emit dispatches a rendered line to the right kservice.Logger method for
// level, the same level->method mapping Handle always used.
func (h *osSlogHandler) emit(level slog.Level, line string) error {
	switch {
	case level >= slog.LevelError: // covers ERROR and our custom FATAL
		return h.logger.Error(line)
	case level >= slog.LevelWarn:
		return h.logger.Warning(line)
	case level >= slog.LevelInfo:
		return h.logger.Info(line)
	case level >= slog.LevelDebug:
		return h.logger.Info("DEBUG " + line)
	default: // our custom TRACE level, see the design note above
		return h.logger.Info("TRACE " + line)
	}
}

// decideFileLogger resolves the buffer built up so far -- replayed if there's
// no file logger, discarded otherwise -- and only the first call counts; see
// program.go, which calls this from CLIOptOnConfigParse and, defensively,
// once more after RunCLI returns in case that hook never fired.
func (h *osSlogHandler) decideFileLogger(hasFileLogger bool) {
	st := h.state
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.decided {
		return
	}
	st.decided = true
	st.forward = !hasFileLogger

	buf := st.buf
	st.buf = nil
	if st.forward {
		for _, rec := range buf {
			_ = h.emit(rec.level, rec.line)
		}
	}
	// Otherwise: the file logger already has these records on disk.
}

func writeAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		nested := append(append([]string{}, groups...), a.Key)
		for _, ga := range a.Value.Group() {
			writeAttr(b, nested, ga)
		}
		return
	}
	b.WriteByte(' ')
	for _, g := range groups {
		b.WriteString(g)
		b.WriteByte('.')
	}
	b.WriteString(a.Key)
	b.WriteByte('=')
	fmt.Fprintf(b, "%v", a.Value.Any())
}

func (h *osSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := &osSlogHandler{logger: h.logger, level: h.level, groups: h.groups, state: h.state}
	n.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return n
}

func (h *osSlogHandler) WithGroup(name string) slog.Handler {
	n := &osSlogHandler{logger: h.logger, level: h.level, attrs: h.attrs, state: h.state}
	n.groups = append(append([]string{}, h.groups...), name)
	return n
}

// parseLogLevel maps a logger.level config string onto an slog.Level, the
// same set of names Bento's own internal/log.parseLevel accepts (that
// function is unexported, so this mirrors it rather than importing it).
func parseLogLevel(level string) slog.Level {
	const (
		levelTrace = slog.Level(-8)
		levelFatal = slog.Level(12)
		levelOff   = slog.Level(16)
	)
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "OFF", "NONE":
		return levelOff
	case "FATAL":
		return levelFatal
	case "ERROR":
		return slog.LevelError
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "INFO":
		return slog.LevelInfo
	case "DEBUG":
		return slog.LevelDebug
	case "TRACE", "ALL":
		return levelTrace
	default:
		return slog.LevelInfo
	}
}
