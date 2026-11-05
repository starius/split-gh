package serve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditLogCap(t *testing.T) {
	home := t.TempDir()
	if err := appendAudit(home, map[string]any{"phase": "start"}); err != nil {
		t.Fatal(err)
	}
	// A sparse file over the cap: serve refuses instead of dropping records.
	if err := os.Truncate(filepath.Join(home, ".local/state/split-gh/log.jsonl"), maxAuditBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := appendAudit(home, map[string]any{"phase": "start"}); !errors.Is(err, errAuditFull) {
		t.Fatalf("got %v", err)
	}
}

func TestAuditRecordSizeIsBounded(t *testing.T) {
	home := t.TempDir()
	argv := make([]string, 100)
	for i := range argv {
		argv[i] = strings.Repeat("<", 1000)
	}
	if err := appendAudit(home, map[string]any{"phase": "start", "argv": argv}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".local/state/split-gh/log.jsonl"))
	if len(b) > 6000 || strings.Contains(string(b), "u003c") {
		t.Fatalf("record of %d bytes: %.200s", len(b), b)
	}
}
