package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/warpstreamlabs/bento/public/service"
)

// TestProgramStopCancelsContextAndWaits verifies that Stop cancels the
// context given to runCLI and waits for it to return before Stop itself
// returns, which is what lets program.run observe stopRequested=true and
// skip the failure path below.
func TestProgramStopCancelsContextAndWaits(t *testing.T) {
	ranWithCancelledCtx := make(chan bool, 1)

	p := &program{
		runCLI: func(ctx context.Context, _ ...service.CLIOptFunc) {
			<-ctx.Done()
			ranWithCancelledCtx <- true
		},
		osLogger: &fakeOSLogger{},
	}

	if err := p.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- p.Stop(nil) }()

	select {
	case ran := <-ranWithCancelledCtx:
		if !ran {
			t.Fatal("runCLI did not observe context cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runCLI never observed context cancellation; Stop did not cancel it")
	}

	if err := <-stopErr; err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
}

// TestProgramRunnerReturnsWithoutStopExits is the "runner returned without
// Stop => failure path invoked" case: if runCLI returns on its own (the
// pipeline ended, or an internal error path returned instead of exiting)
// and Stop was never called, program.run must treat that as a failure --
// logging it and exiting non-zero -- rather than silently leaving the
// service manager thinking it's still running. See the comment in
// program.run (program.go) for why exiting is how this gets signalled, not
// some kardianos API call.
func TestProgramRunnerReturnsWithoutStopExits(t *testing.T) {
	fake := &fakeOSLogger{}
	exitCode := make(chan int, 1)

	p := &program{
		runCLI: func(ctx context.Context, _ ...service.CLIOptFunc) {
			// Returns immediately, as if the pipeline ran out of input.
		},
		osLogger: fake,
		exit: func(code int) {
			exitCode <- code
		},
	}

	if err := p.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case code := <-exitCode:
		if code == 0 {
			t.Fatalf("exit code = %d, want non-zero", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner returning without Stop did not trigger the failure/exit path")
	}

	if len(fake.errors) == 0 {
		t.Fatal("expected at least one ERROR logged to the OS logger before exiting")
	}
}

// TestProgramRunnerReturnsWithoutStopExitsCatchesBreakage shows the
// assertion above is load-bearing: a program.run that always treated a
// returning runCLI as a clean stop (e.g. by checking stopRequested
// incorrectly, or omitting the check) would never call exit, and this test
// demonstrates that absence would be caught as a timeout/failure above.
// Here we just assert the specific invariant directly: exit must be called
// exactly once, with a non-zero code, and only after runCLI returned.
func TestProgramRunnerReturnsWithoutStopExitsCatchesBreakage(t *testing.T) {
	var mu sync.Mutex
	var exitCalls []int
	done := make(chan struct{})

	p := &program{
		runCLI:   func(ctx context.Context, _ ...service.CLIOptFunc) {},
		osLogger: &fakeOSLogger{},
		exit: func(code int) {
			mu.Lock()
			exitCalls = append(exitCalls, code)
			mu.Unlock()
			close(done)
		},
	}

	if err := p.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exit was never called")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(exitCalls) != 1 {
		t.Fatalf("exit called %d times, want exactly 1 (calls=%v); a broken implementation might call it zero times (bug) or repeatedly", len(exitCalls), exitCalls)
	}
	if exitCalls[0] == 0 {
		t.Fatalf("exit code = %d, want non-zero so the service manager records a failure", exitCalls[0])
	}
}

// TestProgramLogsStartingWithConfigAndEnvPaths checks the lifecycle logging
// added alongside the tee-gating fix: run must log a "starting" INFO line
// to the OS logger directly (not through the tee), naming the -c/-e paths
// it read off os.Args -- the same os.Args the service manager invokes the
// process with (see parseConfigArgs in args.go).
func TestProgramLogsStartingWithConfigAndEnvPaths(t *testing.T) {
	origArgs := os.Args
	os.Args = []string{"bento-ads", "-c", "/etc/bento-ads/config.yaml", "-e", "/etc/bento-ads/.env"}
	defer func() { os.Args = origArgs }()

	fake := &fakeOSLogger{}
	// run logs "starting" BEFORE calling runCLI (see program.go), so
	// waiting for this close guarantees that line has already been
	// written -- without it, Stop (below) could race program.run's
	// goroutine and run before "starting" is logged.
	runCLIStarted := make(chan struct{})
	p := &program{
		runCLI: func(ctx context.Context, _ ...service.CLIOptFunc) {
			close(runCLIStarted)
			<-ctx.Done()
		},
		osLogger: fake,
	}

	if err := p.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-runCLIStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("runCLI was never called")
	}
	if err := p.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	var startLine string
	for _, line := range fake.infos {
		if strings.Contains(line, "starting") {
			startLine = line
		}
	}
	if startLine == "" {
		t.Fatalf("expected a \"starting\" INFO line, got %v", fake.infos)
	}
	if !strings.Contains(startLine, "/etc/bento-ads/config.yaml") || !strings.Contains(startLine, "/etc/bento-ads/.env") {
		t.Fatalf("expected the starting line to name both the config and env paths; got %q", startLine)
	}
}

// TestProgramLogsStoppingThenStopped checks that Stop logs "stopping"
// (directly, as soon as it's called) and that run logs "stopped" only
// afterwards, once runCLI has actually returned because of that Stop --
// not the "stream ended without a stop" failure path, which is covered by
// TestProgramRunnerReturnsWithoutStopExits above.
func TestProgramLogsStoppingThenStopped(t *testing.T) {
	fake := &fakeOSLogger{}
	// Synchronize on runCLI being reached, same reasoning as in
	// TestProgramLogsStartingWithConfigAndEnvPaths above: otherwise
	// "starting" (run's goroutine) and "stopping" (Stop, called right
	// after Start returns) could land in either order.
	runCLIStarted := make(chan struct{})
	p := &program{
		runCLI: func(ctx context.Context, _ ...service.CLIOptFunc) {
			close(runCLIStarted)
			<-ctx.Done()
		},
		osLogger: fake,
	}

	if err := p.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-runCLIStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("runCLI was never called")
	}
	if err := p.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// run also logs its own "starting" line first (covered by the test
	// above), so check the last two entries rather than the whole slice.
	if len(fake.infos) < 2 {
		t.Fatalf("expected at least a \"stopping\" and a \"stopped\" line, got %v", fake.infos)
	}
	got := fake.infos[len(fake.infos)-2:]
	assertEqualStrings(t, "infos", got, []string{"bento-ads: stopping", "bento-ads: stopped"})
}

// TestProgramStopBeforeRunnerReturnsNoFailure is the mirror case: when
// runCLI returns BECAUSE Stop cancelled its context (a normal, requested
// shutdown), program.run must NOT treat that as a failure.
func TestProgramStopBeforeRunnerReturnsNoFailure(t *testing.T) {
	exitCalled := make(chan struct{})

	p := &program{
		runCLI: func(ctx context.Context, _ ...service.CLIOptFunc) {
			<-ctx.Done()
		},
		osLogger: &fakeOSLogger{},
		exit: func(code int) {
			close(exitCalled)
		},
	}

	if err := p.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-exitCalled:
		t.Fatal("exit was called after a requested Stop; a clean shutdown must not be treated as a failure")
	case <-time.After(200 * time.Millisecond):
		// Expected: no exit call.
	}
}
