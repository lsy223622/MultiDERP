package main

import (
	"fmt"
	"runtime"
)

var (
	uniderpVersion           = "dev"
	gitCommit                = "unknown"
	tailscaleUpstreamVersion = "unknown"
)

func printVersion() {
	fmt.Printf("UniDERP version: %s\n", uniderpVersion)
	fmt.Printf("Git commit: %s\n", gitCommit)
	fmt.Printf("Tailscale upstream version: %s\n", tailscaleUpstreamVersion)
	fmt.Printf("Go version: %s\n", runtime.Version())
}
