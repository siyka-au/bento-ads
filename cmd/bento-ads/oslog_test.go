package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
)

// fakeOSLogger is a fake kservice.Logger that records each call instead of
// writing to a real Windows Event Log / syslog, so the handler/writer
// tests below can run without an OS service. program_test.go's lifecycle
// tests call it from more than one goroutine (program.run logs "starting"
// from its own goroutine while Stop logs "stopping" from the caller's), so
// every method takes mu; tests still rely on channels for ordering, mu
// only guards against torn/concurrent slice writes.
type fakeOSLogger struct {
	mu       sync.Mutex
	errors   []string
	warnings []string
	infos    []string
}

func (f *fakeOSLogger) Error(v ...interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, fmt.Sprint(v...))
	return nil
}

func (f *fakeOSLogger) Warning(v ...interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warnings = append(f.warnings, fmt.Sprint(v...))
	return nil
}

func (f *fakeOSLogger) Info(v ...interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos = append(f.infos, fmt.Sprint(v...))
	return nil
}

func (f *fakeOSLogger) Errorf(format string, a ...interface{}) error {
	return f.Error(fmt.Sprintf(format, a...))
}

func (f *fakeOSLogger) Warningf(format string, a ...interface{}) error {
	return f.Warning(fmt.Sprintf(format, a...))
}

func (f *fakeOSLogger) Infof(format string, a ...interface{}) error {
	return f.Info(fmt.Sprintf(format, a...))
}

func TestOSSlogHandlerLevelMapping(t *testing.T) {
	cases := []struct {
		name     string
		level    slog.Level
		wantErr  []string
		wantWarn []string
		wantInfo []string
	}{
		{name: "error", level: slog.LevelError, wantErr: []string{"boom"}},
		{name: "fatal (custom level above Error)", level: slog.Level(12), wantErr: []string{"boom"}},
		{name: "warn", level: slog.LevelWarn, wantWarn: []string{"boom"}},
		{name: "info", level: slog.LevelInfo, wantInfo: []string{"boom"}},
		{name: "debug", level: slog.LevelDebug, wantInfo: []string{"DEBUG boom"}},
		{name: "trace (custom level below Debug)", level: slog.Level(-8), wantInfo: []string{"TRACE boom"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeOSLogger{}
			h := newOSSlogHandler(fake)
			h.decideFileLogger(false)   // no file logger: forward immediately, as these level-mapping tests assume
			h.setLevel(slog.Level(-16)) // let everything through for this test

			logger := slog.New(h)
			logger.Log(context.Background(), tc.level, "boom")

			assertEqualStrings(t, "errors", fake.errors, tc.wantErr)
			assertEqualStrings(t, "warnings", fake.warnings, tc.wantWarn)
			assertEqualStrings(t, "infos", fake.infos, tc.wantInfo)
		})
	}
}

// TestOSSlogHandlerLevelMappingCatchesBreakage proves the mapping test
// above actually asserts something: if ERROR records were (wrongly) routed
// to Warning instead of Error, this reproduces that bug and shows the
// assertion fails.
func TestOSSlogHandlerLevelMappingCatchesBreakage(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.decideFileLogger(false) // no file logger: forward immediately
	h.setLevel(slog.Level(-16))
	logger := slog.New(h)
	logger.Error("boom")

	if len(fake.errors) != 1 || fake.errors[0] != "boom" {
		t.Fatalf("broken mapping would not have produced exactly one ERROR entry \"boom\"; got errors=%v warnings=%v", fake.errors, fake.warnings)
	}
	// Demonstrate what the broken assertion would look like: asserting
	// Error went to Warning instead would fail, proving the test is
	// sensitive to the mapping.
	if len(fake.warnings) != 0 {
		t.Fatalf("an ERROR record must not also appear as a warning; got %v", fake.warnings)
	}
}

func TestOSSlogHandlerAttributesAsKeyValue(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.decideFileLogger(false) // no file logger: forward immediately
	h.setLevel(slog.LevelInfo)

	logger := slog.New(h)
	logger.With("service", "bento").Info("started", "port", 48898)

	assertEqualStrings(t, "infos", fake.infos, []string{"started service=bento port=48898"})
}

func TestOSSlogHandlerLevelFiltering(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.decideFileLogger(false)  // no file logger: forward immediately
	h.setLevel(slog.LevelInfo) // the Bento default

	logger := slog.New(h)
	logger.Debug("should be dropped")
	logger.Info("should pass")

	assertEqualStrings(t, "infos", fake.infos, []string{"should pass"})

	// Once logger.level is raised to DEBUG (simulating
	// CLIOptOnConfigParse reading logger.level=DEBUG from config),
	// DEBUG records must start reaching the OS log too.
	h.setLevel(slog.LevelDebug)
	logger.Debug("now visible")
	assertEqualStrings(t, "infos", fake.infos, []string{"should pass", "DEBUG now visible"})
}

// TestOSSlogHandlerLevelFilteringCatchesBreakage shows what would happen if
// Enabled ignored the configured level entirely (always returning true):
// the DEBUG record would leak through before logger.level is raised, which
// is exactly the bug this handler must not have, since kardianos's Logger
// has no Debug method of its own to filter for us -- see the comment on
// osSlogHandler in oslog.go for why the filtering must happen here.
func TestOSSlogHandlerLevelFilteringCatchesBreakage(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.setLevel(slog.LevelInfo)

	logger := slog.New(h)
	logger.Debug("leaked")

	if len(fake.infos) != 0 {
		t.Fatalf("a handler that doesn't filter by level would leak DEBUG records through as INFO; got %v", fake.infos)
	}
}

func assertEqualStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s[%d]: got %q, want %q", label, i, got[i], want[i])
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"INFO":    slog.LevelInfo,
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"trace":   slog.Level(-8),
		"TRACE":   slog.Level(-8),
		"all":     slog.Level(-8),
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"fatal":   slog.Level(12),
		"off":     slog.Level(16),
		"none":    slog.Level(16),
		"bogus":   slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLogLevel(in); got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestOSErrWriterSplitsLines(t *testing.T) {
	fake := &fakeOSLogger{}
	w := &osErrWriter{logger: fake}

	n, err := w.Write([]byte("line one\nline two\nline three\n"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len("line one\nline two\nline three\n") {
		t.Fatalf("Write returned n=%d, want full length", n)
	}

	assertEqualStrings(t, "errors", fake.errors, []string{"line one", "line two", "line three"})
}

func TestOSErrWriterTrailingLineWithoutNewline(t *testing.T) {
	fake := &fakeOSLogger{}
	w := &osErrWriter{logger: fake}

	if _, err := w.Write([]byte("partial line, no trailing newline")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	assertEqualStrings(t, "errors", fake.errors, []string{"partial line, no trailing newline"})
}

// TestOSErrWriterCatchesBreakage shows what a broken writer that logged the
// whole multi-line write as a single ERROR entry (instead of one per line)
// would produce, and that the test above would catch it.
func TestOSErrWriterCatchesBreakage(t *testing.T) {
	fake := &fakeOSLogger{}
	w := &osErrWriter{logger: fake}
	_, _ = w.Write([]byte("line one\nline two\n"))

	if len(fake.errors) == 1 {
		t.Fatalf("a writer that doesn't split lines would log exactly one combined ERROR entry here; got %v", fake.errors)
	}
	if len(fake.errors) != 2 {
		t.Fatalf("expected one ERROR entry per line, got %v", fake.errors)
	}
}

// TestOSSlogHandlerFileLoggerSuppressesTee is the core new behavior: once
// decideFileLogger(true) says a file logger is configured, no tee record
// -- at any level -- reaches the OS log, since the file already keeps it.
// Run against the pre-fix handler (no decideFileLogger at all, forwarding
// unconditionally) this is exactly the test that would fail; see the
// handback report for a demonstration against that old behavior.
func TestOSSlogHandlerFileLoggerSuppressesTee(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.setLevel(slog.Level(-16)) // let everything through the level filter
	h.decideFileLogger(true)    // logger.file.path is set

	logger := slog.New(h)
	logger.Error("error record")
	logger.Warn("warn record")
	logger.Info("info record")
	logger.Debug("debug record")

	assertEqualStrings(t, "errors", fake.errors, nil)
	assertEqualStrings(t, "warnings", fake.warnings, nil)
	assertEqualStrings(t, "infos", fake.infos, nil)
}

// TestOSSlogHandlerNoFileLoggerForwardsTee is the mirror case: without a
// file logger, tee records are the only record of what Bento's logger
// emitted (a service's stdout is discarded), so they must still reach the
// OS log, filtered by logger.level as before.
func TestOSSlogHandlerNoFileLoggerForwardsTee(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.setLevel(slog.LevelInfo)
	h.decideFileLogger(false) // logger.file.path is empty/absent

	logger := slog.New(h)
	logger.Debug("should be dropped by the level filter, not the gate")
	logger.Info("should reach the OS log")

	assertEqualStrings(t, "infos", fake.infos, []string{"should reach the OS log"})
}

// TestOSSlogHandlerBuffersUntilDecidedThenForwards covers the ordering gap
// documented on osSlogHandler: records that arrive before
// decideFileLogger has ever been called (as happens for Bento's own
// config-summary/lint lines, logged before OnManagerInitialised runs) must
// not be lost. If the decision turns out to be "no file logger", they are
// flushed in order once decided.
func TestOSSlogHandlerBuffersUntilDecidedThenForwards(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.setLevel(slog.Level(-16))

	logger := slog.New(h)
	logger.Info("buffered before the decision, record one")
	logger.Error("buffered before the decision, record two")

	// Nothing should have reached the OS log yet -- the decision hasn't
	// been made.
	assertEqualStrings(t, "infos", fake.infos, nil)
	assertEqualStrings(t, "errors", fake.errors, nil)

	h.decideFileLogger(false) // no file logger: flush the buffer

	assertEqualStrings(t, "infos", fake.infos, []string{"buffered before the decision, record one"})
	assertEqualStrings(t, "errors", fake.errors, []string{"buffered before the decision, record two"})

	// And subsequent records after the decision still forward normally.
	logger.Info("after the decision")
	assertEqualStrings(t, "infos", fake.infos, []string{
		"buffered before the decision, record one",
		"after the decision",
	})
}

// TestOSSlogHandlerBuffersUntilDecidedThenDiscards is the mirror case: if
// the decision turns out to be "a file logger is configured", the buffered
// records are discarded -- the file already has them -- rather than being
// flushed to the OS log.
func TestOSSlogHandlerBuffersUntilDecidedThenDiscards(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.setLevel(slog.Level(-16))

	logger := slog.New(h)
	logger.Info("buffered before the decision")

	h.decideFileLogger(true) // a file logger is configured: discard

	assertEqualStrings(t, "infos", fake.infos, nil)
	assertEqualStrings(t, "errors", fake.errors, nil)

	// Confirm the gate (not just an empty buffer) is why: a record logged
	// after the decision is also suppressed.
	logger.Info("after the decision")
	assertEqualStrings(t, "infos", fake.infos, nil)
}

// TestOSSlogHandlerDecidesOnlyOnce guards against a second, contradictory
// decideFileLogger call (there should only ever be one -- see program.go,
// which calls it once from CLIOptOnConfigParse and once, defensively,
// after RunCLI returns) silently flipping already-decided behavior.
func TestOSSlogHandlerDecidesOnlyOnce(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.setLevel(slog.Level(-16))

	h.decideFileLogger(true)  // first decision: a file logger is configured
	h.decideFileLogger(false) // must be a no-op

	logger := slog.New(h)
	logger.Info("after both decideFileLogger calls")

	assertEqualStrings(t, "infos", fake.infos, nil)
}

// TestOSSlogHandlerBufferBoundDropsOldest proves the buffer held while
// waiting for the decision is bounded: past maxBufferedRecords, the oldest
// buffered record is dropped (with a single logged note, not one per
// drop) rather than growing without bound.
func TestOSSlogHandlerBufferBoundDropsOldest(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.setLevel(slog.Level(-16))

	logger := slog.New(h)
	// One more than the bound, each uniquely numbered so the survivors can
	// be checked for which end got dropped.
	for i := 0; i < maxBufferedRecords+1; i++ {
		logger.Info(fmt.Sprintf("record %d", i))
	}

	// Exactly one "buffer full" note, logged as a warning the moment the
	// bound was first exceeded -- not one per subsequent drop.
	if len(fake.warnings) != 1 {
		t.Fatalf("expected exactly one buffer-full warning, got %v", fake.warnings)
	}

	h.decideFileLogger(false) // flush what's left

	if len(fake.infos) != maxBufferedRecords {
		t.Fatalf("expected exactly %d flushed records (the bound), got %d", maxBufferedRecords, len(fake.infos))
	}
	if fake.infos[0] != "record 1" {
		t.Fatalf("expected the oldest record (record 0) to have been dropped, leaving \"record 1\" first; got %q", fake.infos[0])
	}
	if fake.infos[len(fake.infos)-1] != fmt.Sprintf("record %d", maxBufferedRecords) {
		t.Fatalf("expected the newest record to survive; got %q last", fake.infos[len(fake.infos)-1])
	}
}

// TestOSSlogHandlerUndecidedFlushOnRunCLIExit simulates program.go's
// defensive decideFileLogger(false) call after RunCLI returns, for the
// case where the CLIOptOnConfigParse hook never fired at all (e.g. the
// config failed to parse before OnManagerInitialised runs): no file logger
// was ever established, so the buffer must still be flushed rather than
// silently lost.
func TestOSSlogHandlerUndecidedFlushOnRunCLIExit(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
	h.setLevel(slog.Level(-16))

	logger := slog.New(h)
	logger.Error("config parse failure, logged before any hook could fire")

	// program.go's post-RunCLI call, unconditionally.
	h.decideFileLogger(false)

	assertEqualStrings(t, "errors", fake.errors, []string{"config parse failure, logged before any hook could fire"})
}
