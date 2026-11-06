package apply

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "a/b"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link": "a/b", "abs": filepath.Join(tmp, "a"), "loop": "loop"} {
		if err := os.Symlink(target, filepath.Join(tmp, name)); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		// ".." goes up from where the symlink leads, not from the symlink.
		tmp + "/link/../c":  tmp + "/a/c",
		tmp + "/abs/b/./x":  tmp + "/a/b/x",
		tmp + "/missing/..": tmp,
		tmp + "//a/":        tmp + "/a",
	} {
		dirs, err := resolve(path)
		if err != nil || len(dirs) == 0 || dirs[len(dirs)-1] != want {
			t.Errorf("resolve(%q) = %q, %v; want it to end at %q", path, dirs, err, want)
		}
	}
	// Every place on the way counts, including the symlink's target.
	dirs, _ := resolve(tmp + "/link/../c")
	if !contains(dirs, tmp+"/a/b") {
		t.Errorf("resolve went around a/b: %q", dirs)
	}
	if _, err := resolve(tmp + "/loop/x"); err == nil {
		t.Error("a symlink loop resolved")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
