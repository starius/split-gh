package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"split-gh/internal/patch"
)

// A patch that leaves part of a change out replays to text that matches it,
// since the format that lost the change loses it again. The trees differ.
// Here the patch is another commit's: the same change to a.txt, without b.txt.
func TestRoundTripComparesTrees(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(files map[string]string) string {
		t.Helper()
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
		git("add", ".")
		git("commit", "--quiet", "-m", "Change a")
		return git("rev-parse", "HEAD")
	}
	git("init", "--quiet")
	git("config", "user.name", "U")
	git("config", "user.email", "u@x")
	base := commit(map[string]string{"a.txt": "a\n"})
	partial := commit(map[string]string{"a.txt": "b\n"})
	git("reset", "--quiet", "--hard", base)
	whole := commit(map[string]string{"a.txt": "b\n", "b.txt": "new\n"})

	texts, err := patch.Format(dir, []string{partial})
	if err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(dir, base, []patch.Commit{{ID: partial, Parents: []string{base}}}, texts); err != nil {
		t.Fatalf("the commit's own patch: %v", err)
	}
	err = roundTrip(dir, base, []patch.Commit{{ID: whole, Parents: []string{base}}}, texts)
	if err == nil || !strings.Contains(err.Error(), "leaves out") {
		t.Fatalf("a patch without b.txt: %v", err)
	}
}
