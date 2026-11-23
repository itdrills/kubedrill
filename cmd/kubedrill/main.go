// Command kubedrill is a local Kubernetes simulator for practising kubectl.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/itskills/kubedrill/internal/cli"
)

// version is set by release builds via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = resolveVersion()
	os.Exit(cli.Execute())
}

// resolveVersion falls back to the module version recorded by
// `go install github.com/itskills/kubedrill/cmd/kubedrill@vX.Y.Z`.
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return version
}
