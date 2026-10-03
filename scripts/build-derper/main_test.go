package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectsChangedUpstreamPin(t *testing.T) {
	for _, change := range []func(*sourceInfo){
		func(s *sourceInfo) { s.Version = "v1.102.4" },
		func(s *sourceInfo) { s.Origin.Hash = strings.Repeat("0", 40) },
		func(s *sourceInfo) { s.Sum = "h1:wrong" },
	} {
		s := sourceInfo{Version: upstreamVersion, Sum: upstreamSum}
		s.Origin.Hash = upstreamCommit
		change(&s)
		if err := validateSource(s); err == nil {
			t.Fatal("accepted changed source pin")
		}
	}
}

func TestRejectsPatchNotApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "derp", "derpserver")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "derpserver.go"), []byte("package derpserver\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyApplied(dir); err == nil {
		t.Fatal("accepted unpatched source")
	}
}
