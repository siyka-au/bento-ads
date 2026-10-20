package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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
// Logger, for use with service.CLIOptAddTeeLogger so that everything
// Bento's own logger emits is *also* visible in the OS system log, which is
// otherwise the only thing a Windows service's stderr/stdout-discarding
// service manager leaves us.
//
// Design notes (see also the package doc comment above main.go's imports
// and program.go's program.run):
//
//   - CLIOptAddTeeLegger wires this in as: cliOpts.opts.OnLoggerInit wraps
//     the real configured logger `l` as log.TeeLogger(l, newAirGapLogger(ourSlogLogger)).
//     internal/log's teeLogger (internal/log/tee.go) is a dumb fan-out: its
//     Error/Warn/Info/Debug methods call straight through to *both* wrapped
//     loggers with no level check of their own. All level filtering happens
//     inside each wrapped logger's own Error/Warn/Info/Debug implementation.
//     That means `l` (the real, configured-by-logger.level logger) decides
//     for itself whether to actually emit a given call, but our tee branch
//     (newAirGapLogger(ourSlogLogger) -> ourSlogLogger.Debug/Info/...)
//     receives *every* call regardless of logger.level, unconditionally.
//     So the filtering against logger.level required by this handler has
//     to happen on our side -- inside Enabled below -- not upstream.
//   - To learn logger.level we use service.CLIOptOnConfigParse (see
//     program.go), which fires once the main config has been parsed, and
//     read the "logger"/"level" field back out of it with
//     ParsedConfig.FieldString. Enabled starts at slog.LevelInfo (Bento's
//     own documented default) until that hook fires, so early output isn't
//     silently dropped if a run never gets that far.
//   - public/service's airGapLogger (public/service/logger.go), which
//     wraps whatever CLIOptAddTeeLogger was given to satisfy log.Modular,
//     forwards TRACE calls to a.l.Trace(...) only if the wrapped type has a
//     literal Trace(string, ...any) method; *slog.Logger doesn't, so
//     TRACE collapses to a plain a.l.Debug(...) call before it ever reaches
//     this handler. The result: TRACE and DEBUG are indistinguishable once
//     they get here -- both arrive as a slog.Record at slog.LevelDebug.
//     There is therefore no way for this handler to tell them apart, and
//     the "DEBUG/TRACE -> Info prefixed with the level" mapping below can
//     only ever produce the "DEBUG" prefix in practice.
//   - Bento's internal loggers call their Debug/Info/Warn/Error methods
//     printf-style, e.g. Debug("reading %d bytes", n); airGapLogger passes
//     that straight through as a.l.Debug(format, v...) with NO
//     fmt.Sprintf applied first (see public/service/logger.go). Called on
//     a real *slog.Logger, Debug(msg string, args ...any) treats `msg` as
//     a literal (unexpanded %d and all) and tries to pair up `v` as
//     alternating key/value attributes, which for printf-style arguments
//     usually isn't an even list of strings -- slog renders the
//     odd/mistyped entries as "!BADKEY". This is an existing rough edge in
//     upstream Bento's airGapLogger/CLIOptAddTeeLogger design (not
//     something introduced here): any caller teeing Bento's log into a
//     real *slog.Logger via a LeveledLogger adapter hits it. We render
//     whatever attrs slog produced rather than trying to recover the
//     original arguments, which are gone by the time Handle sees the
//     Record.
type osSlogHandler struct {
	logger kservice.Logger
	level  *atomic.Int64 // slog.Level, stored as int64
	attrs  []slog.Attr
	groups []string
}

// newOSSlogHandler builds an osSlogHandler defaulting to slog.LevelInfo
// until setLevel is called (normally from a CLIOptOnConfigParse hook, once
// logger.level has actually been read from the config).
func newOSSlogHandler(logger kservice.Logger) *osSlogHandler {
	h := &osSlogHandler{logger: logger, level: &atomic.Int64{}}
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

	switch {
	case r.Level >= slog.LevelError: // covers ERROR and our custom FATAL
		return h.logger.Error(line)
	case r.Level >= slog.LevelWarn:
		return h.logger.Warning(line)
	case r.Level >= slog.LevelInfo:
		return h.logger.Info(line)
	case r.Level >= slog.LevelDebug:
		return h.logger.Info("DEBUG " + line)
	default: // our custom TRACE level, see the design note above
		return h.logger.Info("TRACE " + line)
	}
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
	n := &osSlogHandler{logger: h.logger, level: h.level, groups: h.groups}
	n.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return n
}

func (h *osSlogHandler) WithGroup(name string) slog.Handler {
	n := &osSlogHandler{logger: h.logger, level: h.level, attrs: h.attrs}
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
