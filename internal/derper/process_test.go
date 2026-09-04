package derper

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessHelper(t *testing.T) {
	if os.Getenv("MULTIDERP_PROCESS_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestStopCommandReturnsNilAfterForcedKill(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestProcessHelper")
	command.Env = append(os.Environ(), "MULTIDERP_PROCESS_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stopCommand(ctx, command, done); err != nil {
		t.Fatalf("stopCommand() error after forced kill = %v, want nil", err)
	}
}
