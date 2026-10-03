package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
)

func TestNodeCLIReadsCodeFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code")
	code := strings.Repeat("a", 64)
	if err := os.WriteFile(path, []byte(code+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var got admin.Request
	result := runNodeCLI(func(r admin.Request) (admin.Response, bool) { got = r; return admin.Success("", nil), true }, []string{"enroll", "--controller", "https://controller.example.com", "--code-file", path})
	if result != 0 || got.Action != "node.enroll" || got.ControllerURL != "https://controller.example.com" || got.EnrollmentCode != code {
		t.Fatal("wrong local enrollment request")
	}
	for _, args := range [][]string{{"enroll", "--controller", "https://controller.example.com", "--code", code}, {"enroll", "--controller", "https://controller.example.com?code=secret", "--code-file", path}, {"enroll", "--controller", "http://controller.example.com", "--code-file", path}} {
		called := false
		if got := runNodeCLI(func(admin.Request) (admin.Response, bool) { called = true; return admin.Success("", nil), true }, args); got != 2 || called {
			t.Fatal("unsafe arguments accepted")
		}
	}
}
