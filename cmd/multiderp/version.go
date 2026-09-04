package main

import (
	"fmt"
	"runtime"
)

var (
	multiderpVersion         = "dev"
	gitCommit                = "unknown"
	tailscaleUpstreamVersion = "unknown"
)

func printVersion() {
	fmt.Printf("MultiDERP version: %s\n", multiderpVersion)
	fmt.Printf("Git commit: %s\n", gitCommit)
	fmt.Printf("Tailscale upstream version: %s\n", tailscaleUpstreamVersion)
	fmt.Printf("Go version: %s\n", runtime.Version())
}
