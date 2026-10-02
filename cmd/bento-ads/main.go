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

func main() {
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

	// When launched by the OS service manager, os.Args[1:] is exactly
	// svcConfig.Arguments as recorded at install time -- read the same -c/-e
	// flags back out of it here.
	configPath, envPath := parseConfigArgs(os.Args[1:])
	prg := &program{configPath: configPath, envPath: envPath}

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
		// Launched by the OS service manager.
		if err := svc.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	// Normal interactive (or container) use, unchanged.
	service.RunCLI(
		context.Background(),
		service.CLIOptSetBinaryName("bento-ads"),
		service.CLIOptSetProductName("Bento"),
	)

	if showServiceHelpAfter {
		fmt.Println()
		fmt.Print(serviceHelp)
	}
}
