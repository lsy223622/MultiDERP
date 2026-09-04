package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/lsy223622/MultiDERP/internal/admin"
	"github.com/lsy223622/MultiDERP/internal/config"
	"github.com/lsy223622/MultiDERP/internal/daemon"
	"github.com/lsy223622/MultiDERP/internal/verifier"
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
	configPath := flags.String("config", config.DefaultConfigPath, "path to the required YAML configuration")
	derperBinary := flags.String("derper", "derper", "upstream derper binary")
	admissionAddress := flags.String("admission-address", config.DefaultAdmissionAddress, "internal admission controller address")
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
		ConfigPath:       *configPath,
		DerperBinary:     *derperBinary,
		AdmissionAddress: *admissionAddress,
		DerperOutput:     os.Stdout,
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
	case "tailnet":
		return runTailnetCLI(call, args[1:])
	case "orphan":
		return runOrphanCLI(call, args[1:])
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

func runTailnetCLI(call callFunc, args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return 2
		}
		response, ok := call(admin.Request{Action: "tailnet.list"})
		if !ok {
			return 1
		}
		var statuses []verifier.Status
		if err := json.Unmarshal(response.Data, &statuses); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printStatusList(statuses)
		return 0
	case "status":
		if len(args) < 2 || len(args) > 3 || (len(args) == 3 && args[2] != "--verbose") {
			return 2
		}
		response, ok := call(admin.Request{Action: "tailnet.status", Name: args[1], Verbose: len(args) == 3})
		if !ok {
			return 1
		}
		var status verifier.Status
		if err := json.Unmarshal(response.Data, &status); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printStatus(status)
		return 0
	case "add":
		return addTailnetCLI(call, args[1:])
	case "enable", "disable", "login", "logout", "reset", "remove":
		if len(args) != 2 {
			return 2
		}
		response, ok := call(admin.Request{Action: "tailnet." + args[0], Name: args[1]})
		if !ok {
			return 1
		}
		printAuthURL(response)
		return 0
	}
	usage()
	return 2
}

func addTailnetCLI(call callFunc, args []string) int {
	if len(args) == 0 {
		return 2
	}
	orderedArgs, err := orderTailnetAddArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	flags := flag.NewFlagSet("tailnet add", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	oauthFile := flags.String("oauth-secret-file", "", "Tailscale OAuth client secret file")
	authKeyFile := flags.String("auth-key-file", "", "Tailscale auth key file")
	var tags stringListFlag
	flags.Var(&tags, "tag", "Tailscale tag to advertise (repeatable)")
	required := flags.Bool("required", false, "mark this verifier as required for readiness")
	if err := flags.Parse(orderedArgs); err != nil {
		return 2
	}
	if flags.NArg() != 1 || (*oauthFile != "" && *authKeyFile != "") {
		fmt.Fprintln(os.Stderr, "tailnet add requires exactly one name and at most one authentication secret file")
		return 2
	}
	authType := "web"
	if *oauthFile != "" {
		authType = "oauth"
	}
	if *authKeyFile != "" {
		authType = "auth_key"
	}
	response, ok := call(admin.Request{
		Action:           "tailnet.add",
		Name:             flags.Arg(0),
		AuthType:         authType,
		ClientSecretFile: *oauthFile,
		AuthKeyFile:      *authKeyFile,
		Tags:             append([]string(nil), tags...),
		Required:         required,
	})
	if !ok {
		return 1
	}
	printAuthURL(response)
	return 0
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func orderTailnetAddArgs(args []string) ([]string, error) {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if arg == "--oauth-secret-file" || arg == "--auth-key-file" || arg == "--tag" {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			flags = append(flags, arg, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, "--oauth-secret-file=") || strings.HasPrefix(arg, "--auth-key-file=") || strings.HasPrefix(arg, "--tag=") ||
			arg == "--required" || strings.HasPrefix(arg, "--required=") || strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...), nil
}

func runOrphanCLI(call callFunc, args []string) int {
	if len(args) == 1 && args[0] == "list" {
		response, ok := call(admin.Request{Action: "orphan.list"})
		if !ok {
			return 1
		}
		var items []daemon.OrphanInfo
		if err := json.Unmarshal(response.Data, &items); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, item := range items {
			fmt.Printf("%s\t%s\t%s\t%s\n", item.ID, item.Name, item.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), item.State)
		}
		return 0
	}
	if len(args) >= 2 && args[0] == "purge" {
		confirm := false
		if len(args) == 3 && args[2] == "--yes" {
			confirm = true
		} else if len(args) != 2 {
			return 2
		}
		if !confirm {
			fmt.Printf("Permanently purge orphan state %q? [y/N] ", args[1])
			var answer string
			if _, err := fmt.Scanln(&answer); err != nil || !strings.EqualFold(strings.TrimSpace(answer), "y") {
				fmt.Println("cancelled")
				return 1
			}
			confirm = true
		}
		_, ok := call(admin.Request{Action: "orphan.purge", Name: args[1], Confirm: confirm})
		return boolExit(ok)
	}
	usage()
	return 2
}

func printAuthURL(response admin.Response) {
	var data map[string]any
	if len(response.Data) == 0 || json.Unmarshal(response.Data, &data) != nil {
		return
	}
	if url, ok := data["auth_url"].(string); ok && url != "" {
		fmt.Println("Authentication required.")
		fmt.Println("Send this URL to the Tailnet owner:")
		fmt.Println(url)
	}
}

func printStatusList(statuses []verifier.Status) {
	fmt.Println("NAME\tAUTH\tSTATE\tHARDENING\tADMISSION\tREQUIRED\tEFFECTIVE_REQUIRED")
	for _, status := range statuses {
		hardening := "-"
		if status.HardeningVerified {
			hardening = "secure"
		}
		admissionState := "inactive"
		if status.Admission {
			admissionState = "active"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\n", status.Name, status.Authentication, status.State.String(), hardening, admissionState, yesNo(status.Required), yesNo(status.EffectiveRequired))
	}
}

func printStatus(status verifier.Status) {
	fmt.Printf("Name:           %s\n", status.Name)
	fmt.Printf("Authentication: %s\n", status.Authentication)
	fmt.Printf("State:          %s\n", status.State.String())
	fmt.Printf("Admission:      %s\n", activeInactive(status.Admission))
	fmt.Printf("Required:       %s\n", yesNo(status.Required))
	fmt.Printf("Effective req.: %s\n", yesNo(status.EffectiveRequired))
	if status.AuthURL != "" {
		fmt.Printf("\nAuthentication URL:\n  %s\n", status.AuthURL)
	}
	if status.Tailnet != "" {
		fmt.Printf("\nTailnet:\n  Name:         %s\n", status.Tailnet)
	}
	if status.Node != "" {
		fmt.Printf("  Node:         %s\n", status.Node)
	}
	if status.NodeKey != "" {
		fmt.Printf("  Node Key:     %s\n", status.NodeKey)
	}
	if len(status.TailscaleIPs) > 0 {
		fmt.Printf("  Tailscale IP: %s\n", strings.Join(status.TailscaleIPs, ", "))
	}
	fmt.Printf("\nSecurity:\n  Hardening:    %s\n", activeInactive(status.HardeningVerified))
	if status.HardeningVerified {
		fmt.Println("  Shields Up:          enabled")
		fmt.Println("  Remote Config:       disabled")
		fmt.Println("  Accept Routes:       disabled")
		fmt.Println("  Exit Node:           none")
		fmt.Println("  Advertise Routes:    none")
		fmt.Println("  Advertise Services:  none")
		fmt.Println("  SSH:                 disabled")
		fmt.Println("  Web Client:          disabled")
		fmt.Println("  App Connector:       disabled")
		fmt.Println("  Serve/Funnel:        empty")
		fmt.Println("  Auto Exit Node:      none")
		fmt.Println("  Relay Server Port:   disabled")
		fmt.Println("  Auto Update:         disabled")
	}
	if status.LastError != "" {
		fmt.Printf("\nLast error:     %s\n", status.LastError)
	}
	fmt.Printf("\nState Directory:\n  %s\n", status.StateDirectory)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func activeInactive(value bool) string {
	if value {
		return "active"
	}
	return "inactive"
}

func boolExit(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  multiderp version")
	fmt.Fprintln(os.Stderr, "  multiderp serve [--config path] [--derper binary]")
	fmt.Fprintln(os.Stderr, "  multiderp [--socket path] tailnet list|status [--verbose]|add|enable|disable|login|logout|reset|remove")
	fmt.Fprintln(os.Stderr, "  multiderp [--socket path] orphan list|purge <orphan-id> [--yes]")
	fmt.Fprintln(os.Stderr, "  multiderp [--socket path] config reload")
	fmt.Fprintln(os.Stderr, "  multiderp [--socket path] derp restart")
}
