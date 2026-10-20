package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	kservice "github.com/kardianos/service"
	"github.com/warpstreamlabs/bento/public/service"
)

// stopTimeout bounds how long a graceful shutdown may take before the OS
// service manager's own kill timeout would otherwise force it.
const stopTimeout = 30 * time.Second

// osExit is os.Exit, indirected so a test can substitute a fake and
// observe that the "runner returned without Stop" failure path is taken
// without actually terminating the test process.
var osExit = os.Exit

// program adapts bento-ads's own startup -- the same Bento CLI used by the
// interactive path in main.go -- to kardianos/service's Interface, so that
// running as a service and running interactively go through one startup
// path. os.Args under the service manager is just "-c <cfg> -e <env>"
// (recorded at install time by `service install`), which service.RunCLI
// parses itself, so this type carries no config/env fields of its own.
type program struct {
	// cliOpts are the same CLIOptFuncs passed to the interactive
	// service.RunCLI call in main.go, so that service mode builds and runs
	// the config exactly as an interactive run would.
	cliOpts []service.CLIOptFunc

	// osLogger is the kardianos system logger (Windows Event Log / syslog)
	// that both pre-logger failures (via osErrWriter) and every log line
	// Bento's own logger emits afterwards (via osSlogHandler, teed in
	// alongside Bento's configured logger) are sent to. See oslog.go.
	osLogger kservice.Logger

	// runCLI defaults to service.RunCLI; tests substitute a fake so the
	// lifecycle logic below can be exercised without a real Bento stream.
	runCLI func(ctx context.Context, optFuncs ...service.CLIOptFunc)

	// exit defaults to os.Exit; tests substitute a fake so the "runner
	// returned without Stop" failure path can be observed without killing
	// the test process.
	exit func(code int)

	mu            sync.Mutex
	cancel        context.CancelFunc
	done          chan struct{}
	stopRequested bool
}

// Start must return quickly, so the actual run happens in its own
// goroutine.
func (p *program) Start(s kservice.Service) error {
	ctx, cancel := context.WithCancel(context.Background())

	p.mu.Lock()
	p.cancel = cancel
	p.done = make(chan struct{})
	p.stopRequested = false
	done := p.done
	p.mu.Unlock()

	go p.run(ctx, done)
	return nil
}

func (p *program) run(ctx context.Context, done chan struct{}) {
	defer close(done)

	runCLI := p.runCLI
	if runCLI == nil {
		runCLI = service.RunCLI
	}

	// bento-ads's own lifecycle, logged directly to the OS logger (not the
	// gated tee below) so it appears regardless of a file logger.
	configPath, envPath := parseConfigArgs(os.Args[1:])
	_ = p.osLogger.Info(fmt.Sprintf(
		"bento-ads service starting (config=%q env=%q version=%s)",
		configPath, envPath, buildVersion(),
	))

	errWriter := &osErrWriter{logger: p.osLogger}
	osSlogHandler := newOSSlogHandler(p.osLogger)
	osSlogLogger := slog.New(osSlogHandler)

	opts := make([]service.CLIOptFunc, 0, len(p.cliOpts)+3)
	opts = append(opts, p.cliOpts...)
	opts = append(opts,
		service.CLIOptSetErrWriter(errWriter),
		service.CLIOptAddTeeLogger(osSlogLogger),
		service.CLIOptOnConfigParse(func(pConf *service.ParsedConfig) error {
			if lvl, err := pConf.FieldString("logger", "level"); err == nil {
				osSlogHandler.setLevel(parseLogLevel(lvl))
			}
			// See osSlogHandler in oslog.go for why this gates the tee.
			filePath, _ := pConf.FieldString("logger", "file", "path")
			osSlogHandler.decideFileLogger(filePath != "")
			return nil
		}),
	)

	// RunCLI blocks until the pipeline ends on its own, a termination
	// signal arrives, or ctx is cancelled (which Stop below does to
	// trigger graceful shutdown within shutdown_timeout).
	runCLI(ctx, opts...)

	// A no-op if the hook above already decided; otherwise it never fired
	// (e.g. the config failed to parse), so flush the buffer as undecided.
	osSlogHandler.decideFileLogger(false)

	p.mu.Lock()
	stopRequested := p.stopRequested
	p.mu.Unlock()

	if stopRequested {
		_ = p.osLogger.Info("bento-ads: stopped")
		return
	}

	// RunCLI returned on its own without Stop being called -- left alone the
	// service would stay recorded as "Running" forever with nothing behind
	// it. kardianos has no call to tell the service manager "this service
	// has failed" (Stop is only ever invoked BY the manager), so the
	// documented way to signal failure is to exit non-zero instead: both
	// Windows' SCM and Linux/BSD service managers record an unannounced,
	// non-zero exit as a failure.
	exit := p.exit
	if exit == nil {
		exit = osExit
	}
	_ = p.osLogger.Error("bento-ads: stream ended without a stop being requested; exiting so the service manager records a failure")
	exit(1)
}

// Stop is called by the OS service manager. It cancels the context RunCLI
// is running under, triggering Bento's graceful shutdown, and waits for
// run's goroutine to finish, up to stopTimeout.
func (p *program) Stop(s kservice.Service) error {
	p.mu.Lock()
	p.stopRequested = true
	cancel := p.cancel
	done := p.done
	p.mu.Unlock()

	_ = p.osLogger.Info("bento-ads: stopping")

	if cancel == nil {
		// Start's goroutine hasn't set up its context yet.
		return nil
	}
	cancel()

	if done == nil {
		return nil
	}
	select {
	case <-done:
	case <-time.After(stopTimeout):
		_ = p.osLogger.Error("bento-ads: graceful shutdown did not finish within stopTimeout")
	}
	return nil
}

// buildVersion returns the binary's VCS revision/build time for the
// "starting" log line (bento's own cli.Version isn't importable, it's
// internal), or "unknown" if no VCS info was embedded.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, buildTime string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			buildTime = s.Value
		}
	}
	if revision == "" {
		return "unknown"
	}
	if buildTime != "" {
		return revision + " (" + buildTime + ")"
	}
	return revision
}
