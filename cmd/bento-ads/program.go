package main

import (
	"context"
	"log/slog"
	"os"
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

// program adapts bento-ads's own startup -- the exact same Bento CLI used
// by the interactive path in main.go -- to kardianos/service's Interface,
// so that running as an OS service and running interactively go through
// one startup path rather than two. (Previously this type built its own
// Stream from the config file via service.NewStreamBuilder().SetYAML,
// a narrower schema than the CLI's that silently rejected CLI-only config
// sections such as telemetry_resources; see the package doc comment above
// main.go's imports.)
//
// os.Args under the service manager is exactly "-c <cfg> -e <env>"
// (Config.Arguments, recorded at install time by `service install` --
// see main.go), which service.RunCLI parses itself via Bento's own CLI,
// the same as it would for an interactive run with those flags. So this
// type carries no config/env path fields of its own.
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
			return nil
		}),
	)

	// RunCLI blocks until the pipeline ends on its own, a termination
	// signal arrives, or ctx is cancelled (which Stop below does to
	// trigger graceful shutdown within shutdown_timeout).
	runCLI(ctx, opts...)

	p.mu.Lock()
	stopRequested := p.stopRequested
	p.mu.Unlock()

	if stopRequested {
		return
	}

	// RunCLI returned on its own -- the pipeline ran out of input and
	// ended, or an error path inside it returned rather than terminating
	// the process -- without Stop ever being called. Left alone, Start's
	// goroutine would simply end here and kardianos would have no reason
	// to tell the service manager anything: the service would stay
	// recorded as "Running" forever with nothing actually running behind
	// it, which is exactly the silent-failure mode this whole change is
	// meant to get rid of.
	//
	// kardianos's Program interface gives no "tell the service manager
	// this service has failed" call -- Stop() is only ever invoked BY the
	// manager, never the other way round, and calling svc.Stop() from
	// inside Start's own goroutine both races the manager's control logic
	// and (on Windows specifically) risks deadlocking the service control
	// handler, which runs Stop on the same dispatcher thread that would
	// need to process our self-issued stop. The documented way to signal
	// an unexpected failure is instead to just exit the process: on
	// Windows, golang.org/x/sys/windows/svc's dispatcher treats a process
	// that exits without having gone through SERVICE_STOP_PENDING /
	// SERVICE_STOPPED as a crash and the SCM records it as stopped due to
	// an error (and will run any configured recovery actions); on
	// Linux/BSD service managers (systemd, upstart, SysV, OpenRC, rc.d) a
	// non-zero exit code with no corresponding stop request is likewise
	// recorded as a failure. So: log it to the OS system log (this is a
	// failure the normal Bento-logger-not-up-yet path doesn't cover
	// either, since Bento's logger may have already shut down normally by
	// this point) and exit non-zero.
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
