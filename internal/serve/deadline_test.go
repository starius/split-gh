package serve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type blockedWriter struct{}

func (blockedWriter) Write([]byte) (int, error) { select {} }

func TestServiceDeadlineStopsBlockedOutput(t *testing.T) {
	if os.Getenv("SPLIT_GH_DEADLINE_HELPER") == "1" {
		ReadService(strings.NewReader("bad\n"), blockedWriter{}, 50*time.Millisecond)
		os.Exit(0)
	}
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServiceDeadlineStopsBlockedOutput$")
	cmd.Env = append(os.Environ(), "SPLIT_GH_DEADLINE_HELPER=1", "HOME="+home, "QREXEC_REMOTE_DOMAIN=test")
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("service did not stop on deadline")
	}
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 1 {
		t.Fatalf("exit: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".local/state/split-gh/log.jsonl"))
	if err != nil || !strings.Contains(string(b), `"phase":"timeout"`) {
		t.Fatalf("timeout audit: %q, %v", b, err)
	}
}
