// Command bento-ads is a Bento distribution with all standard components
// plus the Beckhoff ADS input. It can also install and run itself as an OS
// service (Windows service, systemd/Upstart/SysV/OpenRC on Linux, FreeBSD,
// or launchd on macOS) via `bento-ads service install|uninstall|start|stop|restart`
// -- see README.md's "Running as a service" section.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	kservice "github.com/kardianos/service"
	"github.com/warpstreamlabs/bento/public/service"

	// Import all plugins defined within the Bento repo.
	_ "github.com/warpstreamlabs/bento/public/components/all"

	// Register the ads input.
	_ "github.com/siyka-au/bento-ads"
)

// serviceHelp is printed for `bento-ads service` on its own, `bento-ads
// service -h`/`--help`/`help`, and appended after bento-ads --help/-h/help so
// the service subcommand isn't invisible next to Bento's own CLI help.
// Heading case and indent match Bento's own NAME:/USAGE:/COMMANDS: sections.
const serviceHelp = `SERVICE MANAGEMENT:
   bento-ads service install -c <config> -e <env>   install as an OS service
   bento-ads service uninstall                      remove the installed service
   bento-ads service start                          start the installed service
   bento-ads service stop                           stop the installed service
   bento-ads service restart                        restart the installed service

   -c/-e are only read at install time and are baked into the service
   definition as absolute paths; the other actions need no flags.
`

func isHelpArg(s string) bool {
	return s == "-h" || s == "--help" || s == "help"
}

// commonCLIOpts are the CLIOptFuncs passed to service.RunCLI, identically,
// whether bento-ads is run interactively (or as a container entrypoint) or
// launched by the OS service manager -- see program.go's doc comment and
// program.run, which appends the service-mode-only options (an OS-log
// error writer and tee logger) to this same slice.
func commonCLIOpts() []service.CLIOptFunc {
	return []service.CLIOptFunc{
		service.CLIOptSetBinaryName("bento-ads"),
		service.CLIOptSetProductName("Bento"),
	}
}

// versionArgs are Bento's own --version flag and its -v alias
// (internal/cli/run.go:192), recognised in both single- and double-dash
// form since urfave/cli accepts either.
var versionArgs = map[string]bool{"--version": true, "-version": true, "-v": true, "--v": true}

func main() {
	// Checked before anything else, and only as os.Args[1], so a
	// subcommand's own flags of the same name are unaffected.
	if len(os.Args) > 1 && versionArgs[os.Args[1]] {
		fmt.Println(versionString())
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "service" {
		if len(os.Args) < 3 || isHelpArg(os.Args[2]) {
			fmt.Print(serviceHelp)
			if len(os.Args) < 3 {
				os.Exit(1)
			}
			return
		}
	}
	showServiceHelpAfter := len(os.Args) > 1 && isHelpArg(os.Args[1])

	// `bento-ads service install -c <config> -e <env>` bakes absolute paths
	// into Config.Arguments, since the OS launches the installed service
	// from a working directory we don't control.
	var serviceArgs []string
	if len(os.Args) >= 3 && os.Args[1] == "service" && os.Args[2] == "install" {
		configPath, envPath := parseConfigArgs(os.Args[3:])
		if configPath != "" {
			if abs, err := filepath.Abs(configPath); err == nil {
				configPath = abs
			}
			serviceArgs = append(serviceArgs, "-c", configPath)
		}
		if envPath != "" {
			if abs, err := filepath.Abs(envPath); err == nil {
				envPath = abs
			}
			serviceArgs = append(serviceArgs, "-e", envPath)
		}
	}

	svcConfig := &kservice.Config{
		Name:        "bento-ads",
		DisplayName: "Bento (Beckhoff TwinCAT ADS)",
		Description: "Bento stream processor (bundles the Beckhoff ADS input)",
		Arguments:   serviceArgs,
	}

	// program.run parses the service manager's os.Args itself via
	// service.RunCLI, the same one startup path as an interactive run.
	prg := &program{cliOpts: commonCLIOpts()}

	svc, svcErr := kservice.New(prg, svcConfig)

	if len(os.Args) >= 3 && os.Args[1] == "service" {
		verb := os.Args[2]
		if svcErr != nil {
			fmt.Fprintln(os.Stderr, svcErr)
			os.Exit(1)
		}
		if err := kservice.Control(svc, verb); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if svcErr == nil && !kservice.Interactive() {
		// Service mode: the service manager discards stdout/stderr, so the
		// OS system log is opened here, before anything else, and is the
		// only sink until program.run tees Bento's own logger into it too.
		osLogger, logErr := svc.Logger(nil)
		if logErr != nil {
			// No OS logger to report through; this is the one failure
			// this early that has nowhere to go but stderr.
			fmt.Fprintln(os.Stderr, logErr)
			os.Exit(1)
		}
		prg.osLogger = osLogger

		if err := svc.Run(); err != nil {
			_ = osLogger.Error(err.Error())
			os.Exit(1)
		}
		return
	}

	// Normal interactive (or container) use, unchanged.
	service.RunCLI(context.Background(), commonCLIOpts()...)

	if showServiceHelpAfter {
		fmt.Println()
		fmt.Print(serviceHelp)
	}
}
