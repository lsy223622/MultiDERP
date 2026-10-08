package config

import "testing"

func TestBootstrapConfigRequiresRoleAndHasManagementEntry(t *testing.T) {
	parsed, err := Parse(BootstrapYAML())
	if err != nil {
		t.Fatal(err)
	}
	c := parsed.Config
	if !c.SetupRequired || c.Server.Hostname != "" || c.Server.Management.Listen != ":3378" {
		t.Fatalf("bootstrap unavailable or already configured: %+v", c)
	}
	if c.Server.DERP.Listen != ":3377" || c.Server.DERP.STUNListen != ":3478" || c.Node.DERPPort != 443 || c.Node.STUNPort != 3478 {
		t.Fatalf("unexpected deployment defaults: %+v", c)
	}
	if len(parsed.Warnings) != 0 {
		t.Fatal(parsed.Warnings)
	}
}

func TestExplicitRolesAndUnjoinedMemberConfig(t *testing.T) {
	for _, input := range []string{
		"version: 2\ncontroller:\n  enabled: true\n",
		"version: 2\ncontroller:\n  enabled: false\n",
		"version: 2\ncontroller:\n  enabled: false\nnode:\n  controller_url: https://control.example.com\n",
	} {
		parsed, err := Parse([]byte(input))
		if err != nil || parsed.Config.SetupRequired {
			t.Fatalf("explicit role config: %v %+v", err, parsed)
		}
	}
	for _, input := range []string{
		"version: 2\ncontroller:\n  enabled: false\nnode:\n  controller_url: http://control.example.com\n",
		"version: 2\nnode:\n  derp_port: -1\n",
		"version: 2\nnode:\n  stun_port: 65536\n",
		"version: 2\nserver:\n  management:\n    listen: not-an-address\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatalf("invalid bootstrap fields accepted: %s", input)
		}
	}
}
