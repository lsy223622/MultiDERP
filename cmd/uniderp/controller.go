package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
)

func runControllerCLI(call callFunc, args []string) int {
	if len(args) == 0 || (args[0] != "init" && args[0] != "recover") {
		return 2
	}
	flags := flag.NewFlagSet("controller "+args[0], flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	username := flags.String("username", "", "initial administrator username")
	userID := flags.String("user-id", "", "administrator ID to recover")
	path := flags.String("password-file", "", "protected file containing the administrator password")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *path == "" || (args[0] == "init" && (*username == "" || *userID != "")) || (args[0] == "recover" && (*userID == "" || *username != "")) {
		return 2
	}
	file, err := os.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open password file")
		return 1
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 75))
	if err != nil || len(data) > 74 {
		fmt.Fprintln(os.Stderr, "invalid password file")
		return 1
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if len(password) < 12 || len(password) > 72 {
		fmt.Fprintln(os.Stderr, "password must contain 12 to 72 bytes")
		return 1
	}
	response, ok := call(admin.Request{Action: "controller." + args[0], Username: *username, UserID: *userID, Password: password})
	if !ok {
		return 1
	}
	if args[0] == "init" {
		fmt.Println(string(response.Data))
	}
	return 0
}
