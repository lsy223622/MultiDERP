package config

import (
	"strings"
	"testing"
)

func TestClusterRoleConfigurations(t *testing.T) {
	for _, input := range []string{
		"version: 2\ncontroller:\n  enabled: true\nserver:\n  hostname: derp.example.com\n",
		"version: 2\ncontroller:\n  enabled: false\nnode:\n  controller_url: https://control.example.com\nserver:\n  hostname: relay.example.com\n",
	} {
		got, err := Parse([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if got.Config.Node.StateDir == "" {
			t.Fatal("missing node state path")
		}
	}
}

func TestClusterRejectsInvalidLocalConfiguration(t *testing.T) {
	for _, input := range []string{
		"version: 2\ncontroller:\n  enabled: false\nnode:\n  controller_url: http://control.example.com\n",
		"version: 2\ncontroller:\n  enabled: false\n  key_file: /data/secret\nnode:\n  controller_url: https://control.example.com\n",
		"version: 2\ncontroller:\n  enabled: true\n  allowed_node_cidrs: [not-a-range]\n",
		"version: 2\nnode:\n  max_budget_bps: 18446744073709551615\n",
		"version: 2\ntailnets: [{name: alice}]\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestOldSchemaRequiresMigration(t *testing.T) {
	_, err := Parse([]byte("version: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "migrat") {
		t.Fatalf("old schema: %v", err)
	}
}
