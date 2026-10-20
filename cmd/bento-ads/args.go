package main

// parseConfigArgs extracts bento-ads's own -c/-e flag values from args. Used
// to read them back out of a `service install` command line before
// resolving them to absolute paths and baking them into the installed
// service's Config.Arguments (see main.go). The running service reads its
// own -c/-e straight off os.Args via Bento's CLI (service.RunCLI, via
// program.run) rather than through this function.
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
