package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/lsy223622/MultiDERP/internal/admin"
	"github.com/lsy223622/MultiDERP/internal/verifier"
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

func TestTailnetAddForwardsOAuthTagsInOrder(t *testing.T) {
	var got admin.Request
	code := runTailnetCLI(func(request admin.Request) (admin.Response, bool) {
		got = request
		return admin.Success("", nil), true
	}, []string{"add", "company", "--oauth-secret-file", "/run/secrets/company", "--tag", "tag:first", "--tag=tag:second"})
	if code != 0 {
		t.Fatalf("runTailnetCLI() exit code = %d, want 0", code)
	}
	wantTags := []string{"tag:first", "tag:second"}
	if got.Action != "tailnet.add" || got.Name != "company" || got.AuthType != "oauth" || got.ClientSecretFile != "/run/secrets/company" || !reflect.DeepEqual(got.Tags, wantTags) {
		t.Fatalf("OAuth tailnet add request = %#v, want tags %#v", got, wantTags)
	}
}

func TestOrderTailnetAddArgsRejectsMissingTag(t *testing.T) {
	if _, err := orderTailnetAddArgs([]string{"company", "--tag"}); err == nil {
		t.Fatal("orderTailnetAddArgs() accepted a missing tag value")
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
