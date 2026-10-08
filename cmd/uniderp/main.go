package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/daemon"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	if args[0] == "version" {
		if len(args) != 1 {
			usage()
			return 2
		}
		printVersion()
		return 0
	}
	if args[0] == "serve" {
		return runServe(args[1:])
	}
	socket, remaining, err := parseSocket(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return runCLI(socket, remaining)
}

func runServe(args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", config.DefaultConfigPath, "path to the YAML configuration")
	derperBinary := flags.String("derper", "derper", "patched derper binary")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "serve does not accept positional arguments")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	daemonInstance := daemon.New(ctx, daemon.Options{
		ConfigPath:   *configPath,
		DerperBinary: *derperBinary,
		DerperOutput: os.Stdout,
	})
	if err := daemonInstance.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func parseSocket(args []string) (string, []string, error) {
	socket := config.DefaultAdminSocket
	for len(args) > 0 {
		switch {
		case args[0] == "--socket":
			if len(args) < 2 {
				return "", nil, errors.New("--socket requires a path")
			}
			socket = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--socket="):
			socket = strings.TrimPrefix(args[0], "--socket=")
			args = args[1:]
		default:
			return socket, args, nil
		}
	}
	return socket, args, nil
}

func runCLI(socket string, args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	client := admin.Client{SocketPath: socket}
	call := func(request admin.Request) (admin.Response, bool) {
		response, err := client.Call(context.Background(), request)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return response, false
		}
		if response.Message != "" {
			fmt.Println(response.Message)
		}
		return response, true
	}

	switch args[0] {
	case "node":
		return runNodeCLI(call, args[1:])
	case "controller":
		return runControllerCLI(call, args[1:])
	case "config":
		if len(args) == 2 && args[1] == "reload" {
			_, ok := call(admin.Request{Action: "config.reload"})
			return boolExit(ok)
		}
	case "derp":
		if len(args) == 2 && args[1] == "restart" {
			_, ok := call(admin.Request{Action: "derp.restart"})
			return boolExit(ok)
		}
	}
	usage()
	return 2
}

type callFunc func(admin.Request) (admin.Response, bool)

func boolExit(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  uniderp version")
	fmt.Fprintln(os.Stderr, "  uniderp serve [--config path] [--derper binary]")
	fmt.Fprintln(os.Stderr, "  uniderp [--socket path] controller init --username name --password-file path")
	fmt.Fprintln(os.Stderr, "  uniderp [--socket path] node enroll --controller https://controller.example.com --code-file path")
	fmt.Fprintln(os.Stderr, "  uniderp [--socket path] controller recover --user-id id --password-file path")
	fmt.Fprintln(os.Stderr, "  uniderp [--socket path] config reload")
	fmt.Fprintln(os.Stderr, "  uniderp [--socket path] derp restart")
}
