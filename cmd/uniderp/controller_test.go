package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
)

func TestControllerCLIReadsPasswordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	password := " secret administrator password "
	if err := os.WriteFile(path, []byte(password+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ command, flag, value string }{{"init", "--username", "admin"}, {"recover", "--user-id", "uid"}} {
		var got admin.Request
		code := runControllerCLI(func(r admin.Request) (admin.Response, bool) { got = r; return admin.Success("", nil), true }, []string{tc.command, tc.flag, tc.value, "--password-file", path})
		if code != 0 || got.Action != "controller."+tc.command || got.Password != password {
			t.Fatal("invalid local request")
		}
	}
	called := false
	if code := runControllerCLI(func(r admin.Request) (admin.Response, bool) { called = true; return admin.Success("", nil), true }, []string{"init", "--username", "admin", "--password", "plaintext"}); code != 2 || called {
		t.Fatal("plaintext command argument accepted")
	}
}
