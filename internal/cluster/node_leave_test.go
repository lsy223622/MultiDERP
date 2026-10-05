package cluster

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestClearRegistrationPersistsBeforeCacheCleanup(t *testing.T) {
	dir := t.TempDir()
	c, err := NewEnrollmentClient("https://controller.example.com", dir)
	if err != nil {
		t.Fatal(err)
	}
	c.state.Session = NodeSession{ClusterID: "cluster", NodeID: "node", Token: "old"}
	if err := c.saveRegistration(); err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(filepath.Join(dir, "node.key"))
	old, err := c.ClearRegistration()
	if err != nil || old.Token != "old" {
		t.Fatal(old, err)
	}
	cache := filepath.Join(dir, "policy.json")
	os.Mkdir(cache, 0700)
	os.WriteFile(filepath.Join(cache, "blocked"), []byte("keep"), 0600)
	if err := RemoveCache(cache); err == nil {
		t.Fatal("nonempty directory removed")
	}
	reopened, err := NewEnrollmentClient("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := reopened.PolicyBinding(); a != "" || b != "" {
		t.Fatal(a, b)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "node.key"))
	if !bytes.Equal(key, got) {
		t.Fatal("identity changed")
	}
	if err := reopened.SetControllerURL("http://invalid.example.com"); err == nil {
		t.Fatal("HTTP accepted")
	}
	if err := reopened.SetControllerURL("https://new.example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	for _, p := range []string{path, path + ".watermark"} {
		if err := os.WriteFile(p, []byte("cached"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveCache(path); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".watermark"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal(p, err)
		}
	}
	if err := RemoveCache(path); err != nil {
		t.Fatal(err)
	}
}
