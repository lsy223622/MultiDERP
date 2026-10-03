package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	upstreamVersion = "v1.102.3"
	upstreamCommit  = "53a0d659afa51835dd7a9283873cca44261454f8"
	upstreamSum     = "h1:M1czCAtMuIcg+2Z+FBPbJyAk3ZEQGEFKnvHthtE1c6M="
)

type sourceInfo struct {
	Version string
	Sum     string
	Dir     string
	Origin  struct{ Hash string }
}

func validateSource(s sourceInfo) error {
	if s.Version != upstreamVersion || s.Sum != upstreamSum || s.Origin.Hash != upstreamCommit {
		return errors.New("tailscale source pin mismatch")
	}
	return nil
}

func verifyApplied(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "derp", "derpserver", "derpserver.go"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), "uniderpTailnet") {
		return errors.New("UniDERP patch not applied")
	}
	return nil
}

func main() {
	if err := build(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func build() error {
	out := flag.String("out", "derper", "output binary path")
	goos := flag.String("goos", runtime.GOOS, "target operating system")
	goarch := flag.String("goarch", runtime.GOARCH, "target architecture")
	prepare := flag.String("prepare", "", "prepare a fresh source directory without building")
	tests := flag.Bool("test", true, "run patched upstream package tests before building")
	flag.Parse()
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	mod, err := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "tailscale.com").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(mod)) != upstreamVersion {
		return errors.New("go.mod tailscale pin mismatch")
	}
	download, err := exec.Command("go", "mod", "download", "-json", "tailscale.com@"+upstreamVersion).Output()
	if err != nil {
		return err
	}
	var source sourceInfo
	if err := json.Unmarshal(download, &source); err != nil {
		return err
	}
	if err := validateSource(source); err != nil {
		return err
	}
	patchDir := filepath.Join(root, "patches", "tailscale")
	patch, err := os.ReadFile(filepath.Join(patchDir, "uniderp.patch"))
	if err != nil {
		return err
	}
	digest, err := os.ReadFile(filepath.Join(patchDir, "uniderp.sha256"))
	if err != nil {
		return err
	}
	hash := sha256.Sum256(patch)
	if strings.TrimSpace(string(digest)) != hex.EncodeToString(hash[:]) {
		return errors.New("UniDERP patch digest mismatch")
	}
	var work string
	if *prepare != "" {
		work, err = filepath.Abs(*prepare)
		if err == nil {
			err = os.Mkdir(work, 0700)
		}
	} else {
		work, err = os.MkdirTemp("", "uniderp-derper-")
		if err == nil {
			defer os.RemoveAll(work)
		}
	}
	if err != nil {
		return err
	}
	if err := filepath.WalkDir(source.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source.Dir, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(work, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unexpected source entry %s", rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		return errors.Join(copyErr, closeErr)
	}); err != nil {
		return err
	}
	run := func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = work, os.Stdout, os.Stderr
		return cmd.Run()
	}
	if err := run("git", "apply", "--check", filepath.Join(patchDir, "uniderp.patch")); err != nil {
		return err
	}
	if err := run("git", "apply", filepath.Join(patchDir, "uniderp.patch")); err != nil {
		return err
	}
	if err := verifyApplied(work); err != nil {
		return err
	}
	if *prepare != "" {
		fmt.Println(work)
		return nil
	}
	if *tests {
		if err := run("go", "test", "./derp/derpserver", "./cmd/derper"); err != nil {
			return err
		}
	}
	output, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(upstreamVersion, "v")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X tailscale.com/version.longStamp="+version+" -X tailscale.com/version.shortStamp="+version, "-o", output, "./cmd/derper")
	cmd.Dir, cmd.Stdout, cmd.Stderr = work, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+*goos, "GOARCH="+*goarch)
	return cmd.Run()
}
