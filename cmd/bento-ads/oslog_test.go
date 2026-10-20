package main

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
)

// fakeOSLogger is a fake kservice.Logger that records each call instead of
// writing to a real Windows Event Log / syslog, so the handler/writer
// tests below can run without an OS service.
type fakeOSLogger struct {
	errors   []string
	warnings []string
	infos    []string
}

func (f *fakeOSLogger) Error(v ...interface{}) error {
	f.errors = append(f.errors, fmt.Sprint(v...))
	return nil
}

func (f *fakeOSLogger) Warning(v ...interface{}) error {
	f.warnings = append(f.warnings, fmt.Sprint(v...))
	return nil
}

func (f *fakeOSLogger) Info(v ...interface{}) error {
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
	h.setLevel(slog.LevelInfo)

	logger := slog.New(h)
	logger.With("service", "bento").Info("started", "port", 48898)

	assertEqualStrings(t, "infos", fake.infos, []string{"started service=bento port=48898"})
}

func TestOSSlogHandlerLevelFiltering(t *testing.T) {
	fake := &fakeOSLogger{}
	h := newOSSlogHandler(fake)
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
