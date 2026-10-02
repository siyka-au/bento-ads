package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	kservice "github.com/kardianos/service"
	"github.com/warpstreamlabs/bento/public/service"
)

// stopTimeout bounds how long a graceful shutdown may take before the OS
// service manager's own kill timeout would otherwise force it.
const stopTimeout = 30 * time.Second

// program adapts a Bento stream, built from a config file and optional .env
// file, to kardianos/service's Interface. It is only used on the
// OS-service-managed path; an interactive run goes through service.RunCLI in
// main.go unchanged, since RunCLI's graceful shutdown depends on a console
// signal or an unexpired context deadline that a service doesn't have.
type program struct {
	configPath string
	envPath    string

	mu     sync.Mutex
	stream *service.Stream
}

// Start must return quickly, so the actual stream runs in its own goroutine.
func (p *program) Start(s kservice.Service) error {
	go p.run()
	return nil
}

func (p *program) run() {
	if p.envPath != "" {
		if err := loadEnvFile(p.envPath); err != nil {
			fmt.Fprintf(os.Stderr, "bento-ads: loading %s: %v\n", p.envPath, err)
			return
		}
	}

	conf, err := os.ReadFile(p.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bento-ads: reading %s: %v\n", p.configPath, err)
		return
	}

	builder := service.NewStreamBuilder()
	if err := builder.SetYAML(string(conf)); err != nil {
		fmt.Fprintf(os.Stderr, "bento-ads: parsing %s: %v\n", p.configPath, err)
		return
	}

	stream, err := builder.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bento-ads: building stream: %v\n", err)
		return
	}

	p.mu.Lock()
	p.stream = stream
	p.mu.Unlock()

	if err := stream.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "bento-ads: stream stopped: %v\n", err)
	}
}

// Stop is called by the OS service manager; it must not block for long, and
// must not call os.Exit.
func (p *program) Stop(s kservice.Service) error {
	p.mu.Lock()
	stream := p.stream
	p.mu.Unlock()
	if stream == nil {
		// Start's goroutine hasn't finished building the stream yet.
		return nil
	}
	return stream.StopWithin(stopTimeout)
}

// loadEnvFile sets each KEY=VALUE line of path into the process environment,
// mirroring bento-ads's own -e flag for the interactive path (a service has
// no shell to source a .env file for it). Blank lines and lines starting
// with "#" are skipped.
func loadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// parseConfigArgs extracts bento-ads's own -c/-e flag values from args. Used
// both to read the config/env paths an OS-launched service was installed
// with (where args is os.Args[1:], exactly the Config.Arguments recorded at
// install time) and to read them back out of a `service install` command
// line before resolving them to absolute paths.
func parseConfigArgs(args []string) (configPath, envPath string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		case "-e":
			if i+1 < len(args) {
				envPath = args[i+1]
				i++
			}
		}
	}
	return
}
