package main

import (
	"fmt"
	"runtime"
)

var (
	uniderpVersion           = "dev"
	gitCommit                = "unknown"
	tailscaleUpstreamVersion = "unknown"
	tailscalePatchID         = "unknown"
)

func printVersion() {
	fmt.Printf("UniDERP version: %s\n", uniderpVersion)
	fmt.Printf("Git commit: %s\n", gitCommit)
	fmt.Printf("Tailscale upstream version: %s\n", tailscaleUpstreamVersion)
	fmt.Printf("Tailscale patch SHA256: %s\n", tailscalePatchID)
	fmt.Printf("Go version: %s\n", runtime.Version())
}
