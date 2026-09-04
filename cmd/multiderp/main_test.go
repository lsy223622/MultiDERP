package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"multiderp/internal/admin"
	"multiderp/internal/verifier"
)

func TestOrderTailnetAddArgsAllowsNameBeforeFlags(t *testing.T) {
	got, err := orderTailnetAddArgs([]string{"alice", "--oauth-secret-file", "/run/secrets/alice", "--required"})
	if err != nil {
		t.Fatalf("orderTailnetAddArgs() error = %v", err)
	}
	want := []string{"--oauth-secret-file", "/run/secrets/alice", "--required", "alice"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderTailnetAddArgs() = %#v, want %#v", got, want)
	}
}

func TestTailnetStatusVerboseFlagIsForwarded(t *testing.T) {
	var got admin.Request
	statusData, err := json.Marshal(verifier.Status{Name: "alice"})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	code := runTailnetCLI(func(request admin.Request) (admin.Response, bool) {
		got = request
		return admin.Success("", json.RawMessage(statusData)), true
	}, []string{"status", "alice", "--verbose"})
	if code != 0 {
		t.Fatalf("runTailnetCLI() exit code = %d, want 0", code)
	}
	if !got.Verbose || got.Action != "tailnet.status" || got.Name != "alice" {
		t.Fatalf("verbose status request = %#v", got)
	}
}

func TestOrderTailnetAddArgsRejectsMissingSecretPath(t *testing.T) {
	if _, err := orderTailnetAddArgs([]string{"alice", "--auth-key-file"}); err == nil {
		t.Fatal("orderTailnetAddArgs() accepted a missing secret path")
	}
}
