package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
)

func runNodeCLI(call callFunc, args []string) int {
	if len(args) == 0 || args[0] != "enroll" {
		return 2
	}
	flags := flag.NewFlagSet("node enroll", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	controller := flags.String("controller", "", "controller HTTPS origin")
	path := flags.String("code-file", "", "protected file containing the enrollment code")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	u, err := url.Parse(*controller)
	if flags.NArg() != 0 || *path == "" || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return 2
	}
	u.Path = ""
	f, err := os.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open enrollment code file")
		return 1
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 67))
	if err != nil || len(b) > 66 {
		fmt.Fprintln(os.Stderr, "invalid enrollment code file")
		return 1
	}
	code := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if _, err := hex.DecodeString(code); err != nil || len(code) != 64 {
		fmt.Fprintln(os.Stderr, "invalid enrollment code file")
		return 1
	}
	response, ok := call(admin.Request{Action: "node.enroll", ControllerURL: u.String(), EnrollmentCode: code})
	if !ok {
		return 1
	}
	if len(response.Data) > 0 {
		fmt.Println(string(response.Data))
	}
	return 0
}
