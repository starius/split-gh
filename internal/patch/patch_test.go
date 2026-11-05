package patch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var good = magic + "\nrepo: lnd\nworktree: fix-foo\nbranch: fix/foo\nbase: " + strings.Repeat("a", 40) + "\n\n" +
	Separator + "Subject: one\n\n---\n" + Separator + "Subject: two\n"

func TestParse(t *testing.T) {
	h, patches, err := Parse([]byte(good))
	if err != nil || h.Repo != "lnd" || h.Worktree != "fix-foo" || h.Branch != "fix/foo" || len(patches) != 2 {
		t.Fatalf("%+v %d %v", h, len(patches), err)
	}
	if string(Encode(h, patches)) != good {
		t.Fatal("Encode does not invert Parse")
	}
	bad := map[string]string{
		"version":          strings.Replace(good, magic, "split-gh-patch 2", 1),
		"parent dir":       strings.Replace(good, "repo: lnd", "repo: ../lnd", 1),
		"absolute":         strings.Replace(good, "repo: lnd", "repo: /home/user/lnd", 1),
		"inside .git":      strings.Replace(good, "worktree: fix-foo", "worktree: .git/hooks", 1),
		"nested .git":      strings.Replace(good, "worktree: fix-foo", "worktree: vendor/tool/.git/hooks", 1),
		"nested .GIT":      strings.Replace(good, "worktree: fix-foo", "worktree: vendor/tool/.GIT", 1),
		"repo in .git":     strings.Replace(good, "repo: lnd", "repo: lnd/.git", 1),
		"main checkout":    strings.Replace(good, "worktree: fix-foo", "worktree: .", 1),
		"option branch":    strings.Replace(good, "branch: fix/foo", "branch: -x", 1),
		"shell in branch":  strings.Replace(good, "branch: fix/foo", "branch: x$(id)", 1),
		"escape in branch": strings.Replace(good, "branch: fix/foo", "branch: x\x1b[8m", 1),
		"short base":       strings.Replace(good, strings.Repeat("a", 40), "abc", 1),
		"duplicate":        strings.Replace(good, "repo: lnd\n", "repo: lnd\nrepo: lnd\n", 1),
		"unknown field":    strings.Replace(good, "repo: lnd\n", "repo: lnd\npush: yes\n", 1),
		"missing field":    strings.Replace(good, "branch: fix/foo\n", "", 1),
		"no patches":       strings.SplitAfter(good, "\n\n")[0] + "hello\n",
	}
	for name, data := range bad {
		if _, _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSplitKeepsText(t *testing.T) {
	mbox := Separator + "a\n" + Separator + "b\nFrom 0000 not a separator\n"
	patches, err := Split(mbox)
	if err != nil || len(patches) != 2 || strings.Join(patches, "") != mbox {
		t.Fatalf("%q %v", patches, err)
	}
}

func TestKeyIgnoresOnlyBinaryData(t *testing.T) {
	a := Separator + "Subject: x\n---\ndiff --git a/x b/x\nindex 1111..2222\nGIT binary patch\nliteral 5\nzcmXXX\n\nliteral 0\nHcmV?d00001\n\n" +
		"diff --git a/y b/y\nindex 3333..4444\n--- a/y\n+++ b/y\n@@ -1 +1 @@\n-a\n+b\n"
	if Key(a) != Key(strings.Replace(a, "zcmXXX", "zcmYYY", 1)) {
		t.Fatal("compressed data changed the key")
	}
	for _, change := range [][2]string{{"index 1111..2222", "index 1111..2223"}, {"+b\n", "+c\n"}} {
		if Key(a) == Key(strings.Replace(a, change[0], change[1], 1)) {
			t.Fatalf("%q did not change the key", change[1])
		}
	}
	// The marker in a commit message doesn't hide what follows it.
	msg := Separator + "Subject: x\n\nGIT binary patch\nsecret\n---\n"
	if Key(msg) != msg {
		t.Fatal("message text dropped")
	}
}

func TestVisible(t *testing.T) {
	got := Visible("a\x1b[8mb\u202ec\td\u200b\n\xff")
	if want := "a<U+001B>[8mb<U+202E>c\td<U+200B>\n<0xFF>"; got != want {
		t.Fatalf("%q", got)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(good))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, patches, err := Parse(data)
		if err != nil {
			return
		}
		if h.Check() != nil || len(patches) == 0 {
			t.Fatalf("accepted %+v with %d patches", h, len(patches))
		}
		for _, p := range patches {
			if !strings.HasPrefix(p, Separator) {
				t.Fatalf("patch without separator: %q", p)
			}
		}
	})
}

// Both qubes must format a commit to the same text, whatever their settings:
// split-gh compares the texts of one with those of the other.
func TestFormatIgnoresRepositorySettings(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	lines := make([]string, 60)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
		if i%5 == 0 {
			lines[i] = ""
		}
	}
	git("init", "--quiet")
	git("config", "user.name", "U")
	git("config", "user.email", "u@x")
	write("a.txt", strings.Join(lines, "\n")+"\n")
	write("moved.txt", strings.Repeat("moved\n", 20))
	write("caf\xc3\xa9.txt", "one\n")
	write("z.txt", "z\n")
	git("add", ".")
	git("commit", "--quiet", "-m", "Base")
	// Two hunks 9 lines apart, next to blank lines; a rename; a non-ASCII name.
	lines[11], lines[21] = "changed 11", "changed 21"
	write("a.txt", strings.Join(lines, "\n")+"\n")
	git("mv", "moved.txt", "renamed.txt")
	write("caf\xc3\xa9.txt", "two\n")
	write("z.txt", "zz\n")
	git("commit", "--quiet", "-am", "Change things")
	format := func() string {
		t.Helper()
		texts, err := Format(dir, []string{"HEAD"})
		if err != nil {
			t.Fatal(err)
		}
		return texts[0]
	}
	want := format()
	write("../order", "z.txt\n*\n")
	write("../attributes", "*.txt -diff\n")
	for _, setting := range [][2]string{
		{"diff.interHunkContext", "10"}, {"diff.orderFile", filepath.Join(dir, "../order")}, {"diff.context", "10"},
		{"diff.algorithm", "histogram"}, {"diff.indentHeuristic", "false"}, {"diff.renames", "false"},
		{"diff.renames", "copies"}, {"diff.mnemonicPrefix", "true"}, {"diff.noprefix", "true"},
		{"diff.suppressBlankEmpty", "true"}, {"diff.statNameWidth", "10"}, {"diff.statGraphWidth", "5"},
		{"diff.relative", "true"}, {"core.quotePath", "false"}, {"core.abbrev", "7"}, {"color.ui", "always"},
		{"core.attributesFile", filepath.Join(dir, "../attributes")}, {"format.from", "true"},
		{"format.numbered", "true"}, {"format.subjectPrefix", "X"}, {"format.signOff", "true"},
		{"format.headers", "X-A: b"}, {"format.noprefix", "true"}, {"format.encodeEmailHeaders", "false"},
		{"format.signature", "sig"}, {"format.useAutoBase", "true"}, {"format.mboxrd", "true"},
	} {
		git("config", setting[0], setting[1])
		if got := format(); got != want {
			t.Errorf("%s=%s changes the text:\n%s", setting[0], setting[1], got)
		}
		git("config", "--unset-all", setting[0])
	}

	// Attributes in the checkout too, where git reads them from; old git
	// can't be told to ignore them.
	var major, minor int
	fmt.Sscanf(git("version"), "git version %d.%d", &major, &minor)
	if major < 2 || major == 2 && minor < 40 {
		t.Skip("git before 2.40 reads .gitattributes from the checkout")
	}
	git("config", "diff.lines.xfuncname", "^line 5$")
	for _, attributes := range []string{"*.txt -diff\n", "*.txt diff=lines\n"} {
		write(".gitattributes", attributes)
		if got := format(); got != want {
			t.Errorf(".gitattributes %q changes the text:\n%s", attributes, got)
		}
	}
}
