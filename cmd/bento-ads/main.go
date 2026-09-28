// Command bento-ads is a Bento distribution with all standard components
// plus the Beckhoff ADS input.
package main

import (
	"context"

	"github.com/warpstreamlabs/bento/public/service"

	// Import all plugins defined within the Bento repo.
	_ "github.com/warpstreamlabs/bento/public/components/all"

	// Register the ads input.
	_ "github.com/siyka-au/bento-ads"
)

func main() {
	service.RunCLI(
		context.Background(),
		service.CLIOptSetBinaryName("bento-ads"),
		service.CLIOptSetProductName("Bento"),
	)
}
