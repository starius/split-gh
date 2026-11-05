package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// These tests run the hand-over of commits on one machine: two home
// directories stand in for ai and gh-auth, a bare repository for GitHub, a
// fake qvm-move connects them, and a fake gpg signs.

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

func binDir(t *testing.T) string {
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "split-gh-flow-bin-")
		if buildErr != nil {
			return
		}
		var out []byte
		if out, buildErr = exec.Command("go", "build", "-o", filepath.Join(buildDir, "split-gh"), ".").CombinedOutput(); buildErr != nil {
			buildErr = &buildError{string(out)}
			return
		}
		scripts := map[string]string{
			"qvm-move": "#!/bin/sh\n[ \"$1\" = --without-progress ] && shift\nmkdir -p \"$AUTH_HOME/QubesIncoming/ai\" && cp \"$1\" \"$AUTH_HOME/QubesIncoming/ai/\" && rm \"$1\"\n",
			"fake-gpg": "#!/bin/sh\ncat >/dev/null\necho '[GNUPG:] KEY_CONSIDERED FAKE 0' >&2\necho '[GNUPG:] SIG_CREATED D 1 8 00 1700000000 FAKE' >&2\nprintf '%s\\n' '-----BEGIN PGP SIGNATURE-----' '' 'ZmFrZQ==' '-----END PGP SIGNATURE-----'\n",
		}
		for name, body := range scripts {
			if buildErr = os.WriteFile(filepath.Join(buildDir, name), []byte(body), 0755); buildErr != nil {
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildDir
}

type buildError struct{ out string }

func (e *buildError) Error() string { return "build: " + e.out }

type qubes struct {
	t        *testing.T
	bin      string
	ai, auth string // home directories
	agent    string // the agent's worktree
}

func newQubes(t *testing.T) *qubes {
	q := &qubes{t: t, bin: binDir(t), ai: t.TempDir(), auth: t.TempDir()}
	q.agent = filepath.Join(q.ai, "lnd/fix-foo")
	write(t, filepath.Join(q.ai, ".gitconfig"), "[user]\n\tname = Agent\n\temail = agent@ai\n[init]\n\tdefaultBranch = master\n")
	write(t, filepath.Join(q.auth, ".gitconfig"), "[user]\n\tname = User\n\temail = user@auth\n[gpg]\n\tprogram = "+filepath.Join(q.bin, "fake-gpg")+"\n[init]\n\tdefaultBranch = master\n")
	write(t, filepath.Join(q.auth, ".config/split-gh/config.json"), `{"qubes":{"ai":{"local_repos":["lnd"]}}}`)
	github := filepath.Join(t.TempDir(), "lnd.git")
	q.git(q.auth, "", "init", "--quiet", "--bare", github)
	seed := filepath.Join(t.TempDir(), "seed")
	q.git(q.auth, "", "init", "--quiet", seed)
	write(t, filepath.Join(seed, "README"), "lnd\n")
	q.git(q.auth, seed, "add", "README")
	q.git(q.auth, seed, "commit", "--quiet", "-m", "Initial")
	q.git(q.auth, seed, "push", "--quiet", github, "master")
	q.git(q.auth, "", "clone", "--quiet", github, filepath.Join(q.auth, "lnd"))
	q.git(q.ai, "", "clone", "--quiet", github, filepath.Join(q.ai, "lnd"))
	q.git(q.ai, filepath.Join(q.ai, "lnd"), "worktree", "add", "--quiet", "-b", "fix-foo", "fix-foo", "origin/master")
	return q
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func (q *qubes) env(home string) []string {
	return []string{"HOME=" + home, "PATH=" + q.bin + ":" + os.Getenv("PATH"), "AUTH_HOME=" + q.auth, "GIT_CONFIG_NOSYSTEM=1"}
}

func (q *qubes) run(home, dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = q.env(home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (q *qubes) git(home, dir string, args ...string) string {
	q.t.Helper()
	out, err := q.run(home, dir, "git", args...)
	if err != nil {
		q.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(out)
}

// commit makes a commit in the agent's worktree.
func (q *qubes) commit(file, content, message string) {
	write(q.t, filepath.Join(q.agent, file), content)
	q.git(q.ai, q.agent, "add", file)
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", message)
}

// send runs split-gh send in the agent's worktree and returns the patch path.
func (q *qubes) send(args ...string) string {
	q.t.Helper()
	out, err := q.run(q.ai, q.agent, filepath.Join(q.bin, "split-gh"), append([]string{"send"}, args...)...)
	if err != nil {
		q.t.Fatalf("send: %v\n%s", err, out)
	}
	if !strings.Contains(out, "split-gh apply ~/QubesIncoming/") {
		q.t.Fatalf("send printed no apply command:\n%s", out)
	}
	files, _ := filepath.Glob(filepath.Join(q.auth, "QubesIncoming/ai/*.patch"))
	if len(files) != 1 {
		q.t.Fatalf("expected one patch in QubesIncoming, got %v", files)
	}
	return files[0]
}

func (q *qubes) splitgh(home string, args ...string) (string, error) {
	return q.run(home, home, filepath.Join(q.bin, "split-gh"), args...)
}

func (q *qubes) apply(args ...string) string {
	q.t.Helper()
	out, err := q.splitgh(q.auth, append([]string{"apply"}, args...)...)
	if err != nil {
		q.t.Fatalf("apply: %v\n%s", err, out)
	}
	return out
}

func (q *qubes) authGit(args ...string) string {
	return q.git(q.auth, filepath.Join(q.auth, "lnd"), args...)
}

func TestSendApplyFlow(t *testing.T) {
	q := newQubes(t)
	agent := filepath.Join(q.ai, "lnd/fix-foo")

	// First send creates the worktree and branch, with signed commits.
	q.commit("a.txt", "one\n", "Add a")
	out := q.apply(q.send())
	if !strings.Contains(out, "git push origin refs/heads/fix-foo:refs/heads/fix-foo\n") {
		t.Fatalf("no push command:\n%s", out)
	}
	tip1 := q.authGit("rev-parse", "fix-foo")
	if got := q.git(q.auth, filepath.Join(q.auth, "lnd/fix-foo"), "symbolic-ref", "--short", "HEAD"); got != "fix-foo" {
		t.Fatalf("worktree is on %q", got)
	}
	if q.authGit("rev-parse", "fix-foo^{tree}") != q.git(q.ai, agent, "rev-parse", "HEAD^{tree}") {
		t.Fatal("trees differ")
	}
	if !strings.Contains(q.authGit("cat-file", "commit", "fix-foo"), "\ngpgsig ") {
		t.Fatal("commit is not signed")
	}
	if q.authGit("log", "-1", "--format=%an <%ae> %ad", "refs/heads/fix-foo") != q.git(q.ai, agent, "log", "-1", "--format=%an <%ae> %ad") {
		t.Fatal("author not kept")
	}
	if files, _ := filepath.Glob(filepath.Join(q.auth, "QubesIncoming/ai/*")); len(files) != 0 {
		t.Fatalf("patch not removed: %v", files)
	}

	// A series that only grew keeps the signed commits: a fast-forward.
	q.commit("b.txt", "two\n", "Add b")
	q.apply(q.send())
	if q.authGit("rev-parse", "fix-foo~1") != tip1 {
		t.Fatal("unchanged commit was not kept")
	}

	// The agent rewrites its own last commit: no --force needed.
	write(t, filepath.Join(agent, "b.txt"), "two, fixed\n")
	q.git(q.ai, agent, "commit", "--quiet", "--all", "--amend", "-m", "Add b, fixed")
	q.apply(q.send())
	if q.authGit("rev-parse", "fix-foo~1") != tip1 || q.authGit("log", "-1", "--format=%s", "refs/heads/fix-foo") != "Add b, fixed" {
		t.Fatal("rewrite not applied on the kept commit")
	}

	// The user commits in gh-auth. Applying a patch without that commit is
	// refused unless forced.
	userWT := filepath.Join(q.auth, "lnd/fix-foo")
	write(t, filepath.Join(userWT, "user.txt"), "mine\n")
	q.git(q.auth, userWT, "add", "user.txt")
	q.git(q.auth, userWT, "commit", "--quiet", "-m", "User fix")
	q.commit("c.txt", "three\n", "Add c")
	patchFile := q.send()
	if out, err := q.splitgh(q.auth, "apply", patchFile); err == nil || !strings.Contains(out, "User fix") || !strings.Contains(out, "push") {
		t.Fatalf("apply dropped the user's commit: %v\n%s", err, out)
	}
	diff, err := q.splitgh(q.auth, "diff", patchFile)
	if err != nil || !strings.Contains(diff, "apply needs --force") || !strings.Contains(diff, "-Subject: User fix") || !strings.Contains(diff, "+Subject: Add c") {
		t.Fatalf("diff: %v\n%s", err, diff)
	}
	os.Remove(patchFile)

	// The user pushes, and the agent builds on the user's commit: a
	// fast-forward again.
	userTip := q.authGit("rev-parse", "fix-foo")
	q.authGit("push", "--quiet", "origin", "fix-foo")
	q.git(q.ai, agent, "fetch", "--quiet", "origin")
	q.git(q.ai, agent, "reset", "--quiet", "--hard", "origin/fix-foo")
	q.commit("c.txt", "three\n", "Add c")
	q.apply(q.send())
	if q.authGit("rev-parse", "fix-foo~1") != userTip || q.authGit("rev-parse", "fix-foo^{tree}") != q.git(q.ai, agent, "rev-parse", "HEAD^{tree}") {
		t.Fatal("agent's commit not added on the user's")
	}
	if q.authGit("rev-parse", "refs/split-gh/applied/fix-foo") != q.authGit("rev-parse", "fix-foo") {
		t.Fatal("applied marker not updated")
	}
}

func TestSendRefusals(t *testing.T) {
	q := newQubes(t)
	agent := filepath.Join(q.ai, "lnd/fix-foo")
	splitgh := filepath.Join(q.bin, "split-gh")
	if out, err := q.run(q.ai, filepath.Join(q.ai, "lnd"), splitgh, "send"); err == nil || !strings.Contains(out, "main checkout") {
		t.Fatalf("main checkout: %v\n%s", err, out)
	}
	if out, err := q.run(q.ai, agent, splitgh, "send"); err == nil || !strings.Contains(out, "nothing to send") {
		t.Fatalf("empty series: %v\n%s", err, out)
	}
	// git am would end this message at the "---" line.
	q.commit("a.txt", "one\n", "Add a\n\nBody\n---\nMore body")
	if out, err := q.run(q.ai, agent, splitgh, "send"); err == nil || !strings.Contains(out, "reword") {
		t.Fatalf("message that doesn't survive git am: %v\n%s", err, out)
	}
}

func TestApplyRefusals(t *testing.T) {
	q := newQubes(t)
	q.commit("a.txt", "one\n", "Add a")
	patchFile := q.send()
	userWT := filepath.Join(q.auth, "lnd/fix-foo")

	// The same path holds a directory that isn't the branch's worktree.
	write(t, filepath.Join(userWT, "notes"), "not a worktree\n")
	if out, err := q.splitgh(q.auth, "apply", patchFile); err == nil || !strings.Contains(out, "not a worktree") {
		t.Fatalf("plain directory: %v\n%s", err, out)
	}
	os.RemoveAll(userWT)

	// The repository is not in local_repos.
	write(t, filepath.Join(q.auth, ".config/split-gh/config.json"), `{"qubes":{"ai":{"local_repos":["other"]}}}`)
	if out, err := q.splitgh(q.auth, "apply", patchFile); err == nil || !strings.Contains(out, "not in local_repos") {
		t.Fatalf("repository not allowed: %v\n%s", err, out)
	}
	write(t, filepath.Join(q.auth, ".config/split-gh/config.json"), `{"qubes":{"ai":{"local_repos":["lnd"]}}}`)

	// Uncommitted changes in the worktree are never overwritten.
	q.apply(patchFile)
	write(t, filepath.Join(userWT, "a.txt"), "edited\n")
	q.commit("b.txt", "two\n", "Add b")
	if out, err := q.splitgh(q.auth, "apply", q.send()); err == nil || !strings.Contains(out, "uncommitted changes") {
		t.Fatalf("dirty worktree: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(userWT, "a.txt")); string(got) != "edited\n" {
		t.Fatalf("user's edit lost: %q", got)
	}

	// A patch outside ~/QubesIncoming/<qube>/ has no known sender.
	elsewhere := filepath.Join(q.auth, "x.patch")
	write(t, elsewhere, "")
	if out, err := q.splitgh(q.auth, "apply", elsewhere); err == nil || !strings.Contains(out, "QubesIncoming") {
		t.Fatalf("patch outside QubesIncoming: %v\n%s", err, out)
	}
}

func TestDiffMakesHiddenTextVisible(t *testing.T) {
	q := newQubes(t)
	q.commit("a.txt", "safe\x1b[8m hidden\x1b[0m \u202eevil\n", "Add a")
	patchFile := q.send()
	out, err := q.splitgh(q.auth, "diff", patchFile)
	if err != nil {
		t.Fatalf("diff: %v\n%s", err, out)
	}
	if strings.Contains(out, "\x1b[8m") || strings.Contains(out, "\u202e") {
		t.Fatal("diff passed hidden text through")
	}
	if !strings.Contains(out, "<U+001B>[8m hidden") || !strings.Contains(out, "<U+202E>evil") {
		t.Fatalf("hidden text not shown:\n%s", out)
	}
}

func TestFailedApplyChangesNothing(t *testing.T) {
	q := newQubes(t)
	userWT := filepath.Join(q.auth, "lnd/fix-foo")

	// Signing fails on the first apply: no worktree, no branch.
	q.commit("a.txt", "one\n", "Add a")
	patchFile := q.send()
	gitconfig := filepath.Join(q.auth, ".gitconfig")
	good, _ := os.ReadFile(gitconfig)
	write(t, gitconfig, strings.Replace(string(good), filepath.Join(q.bin, "fake-gpg"), "false", 1))
	if out, err := q.splitgh(q.auth, "apply", patchFile); err == nil || !strings.Contains(out, "git am failed") {
		t.Fatalf("apply without a signature: %v\n%s", err, out)
	}
	if _, err := os.Stat(userWT); !os.IsNotExist(err) || q.authGit("branch", "--list", "fix-foo") != "" {
		t.Fatal("failed apply left a worktree or branch")
	}
	write(t, gitconfig, string(good))
	q.apply(patchFile)
	tip := q.authGit("rev-parse", "refs/heads/fix-foo")

	// An untracked file in the way of the next patch: everything stays put.
	write(t, filepath.Join(userWT, "b.txt"), "untracked\n")
	q.commit("b.txt", "two\n", "Add b")
	if out, err := q.splitgh(q.auth, "apply", q.send()); err == nil || !strings.Contains(out, "git am failed") {
		t.Fatalf("apply over an untracked file: %v\n%s", err, out)
	}
	if q.authGit("rev-parse", "refs/heads/fix-foo") != tip || q.git(q.auth, userWT, "symbolic-ref", "--short", "HEAD") != "fix-foo" {
		t.Fatal("failed apply moved the branch or left the worktree detached")
	}
	if b, _ := os.ReadFile(filepath.Join(userWT, "b.txt")); string(b) != "untracked\n" {
		t.Fatalf("untracked file changed: %q", b)
	}
}

// The worktree holds the agent's files. A relative core.hooksPath or
// core.fsmonitor would make git run programs from it.
func TestApplyRunsNoHooks(t *testing.T) {
	q := newQubes(t)
	ran := filepath.Join(q.auth, "ran")
	q.authGit("config", "core.hooksPath", ".githooks")
	q.authGit("config", "core.fsmonitor", ".githooks/fsmonitor")
	for _, name := range []string{"post-checkout", "reference-transaction", "fsmonitor"} {
		hook := filepath.Join(q.agent, ".githooks", name)
		write(t, hook, "#!/bin/sh\necho "+name+" >> "+ran+"\n")
		if err := os.Chmod(hook, 0755); err != nil {
			t.Fatal(err)
		}
	}
	q.git(q.ai, q.agent, "add", ".githooks")
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", "Add hooks")
	q.apply(q.send())
	// Now the worktree in gh-auth has the hooks, for the next apply.
	q.commit("a.txt", "one\n", "Add a")
	q.apply(q.send())
	if b, err := os.ReadFile(ran); err == nil {
		t.Fatalf("apply ran the agent's programs:\n%s", b)
	}
}

// The push command names the branch fully: in git push, "+master" means
// force-push master, and "heads/master" would be master too.
func TestPushCommandPushesOnlyTheBranch(t *testing.T) {
	q := newQubes(t)
	github := q.authGit("remote", "get-url", "origin")
	remote := func(ref string) string {
		return q.git(q.auth, "", "--git-dir="+github, "rev-parse", "--verify", "--quiet", ref)
	}
	q.git(q.ai, q.agent, "branch", "-m", "+master")
	// The user's master in gh-auth differs from GitHub's.
	q.authGit("commit", "--quiet", "--amend", "-m", "Not for GitHub")
	master := remote("refs/heads/master")
	push := func(out string) {
		t.Helper()
		_, line, ok := strings.Cut(out, "Push it yourself:\n  ")
		line, _, _ = strings.Cut(line, "\n")
		if !ok || !strings.Contains(line, "refs/heads/+master:refs/heads/+master") {
			t.Fatalf("push command:\n%s", out)
		}
		if out, err := q.run(q.auth, q.auth, "sh", "-c", line); err != nil {
			t.Fatalf("%s: %v\n%s", line, err, out)
		}
		if remote("refs/heads/master") != master || remote("refs/heads/+master") != q.authGit("rev-parse", "refs/heads/+master") {
			t.Fatalf("%s pushed the wrong thing", line)
		}
	}
	q.commit("a.txt", "one\n", "Add a")
	push(q.apply(q.send()))
	// A rewrite: the command is a --force-with-lease push.
	q.git(q.ai, q.agent, "commit", "--quiet", "--amend", "-m", "Add a, reworded")
	out := q.apply(q.send())
	if !strings.Contains(out, "--force-with-lease=refs/heads/+master:") {
		t.Fatalf("no lease:\n%s", out)
	}
	push(out)
}

// apply moves the branch and its marker together, or neither. Here the
// marker can't be written: one for a deleted branch fix-foo is in its way.
func TestApplyMovesBranchAndMarkerTogether(t *testing.T) {
	q := newQubes(t)
	q.commit("a.txt", "one\n", "Add a")
	q.apply(q.send())
	q.authGit("worktree", "remove", "fix-foo")
	q.authGit("branch", "-D", "fix-foo")
	main := filepath.Join(q.ai, "lnd")
	q.git(q.ai, main, "worktree", "remove", "--force", "fix-foo")
	q.git(q.ai, main, "branch", "-D", "fix-foo")
	q.git(q.ai, main, "worktree", "add", "--quiet", "-b", "fix-foo/bar", "bar", "origin/master")
	q.agent = filepath.Join(main, "bar")
	q.commit("b.txt", "two\n", "Add b")
	patchFile := q.send()
	if out, err := q.splitgh(q.auth, "apply", patchFile); err == nil || !strings.Contains(out, "refs/split-gh/applied/fix-foo") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if q.authGit("branch", "--list", "fix-foo/bar") != "" {
		t.Fatal("failed apply created the branch")
	}
	if _, err := os.Stat(filepath.Join(q.auth, "lnd/bar")); !os.IsNotExist(err) {
		t.Fatal("failed apply left the worktree")
	}
	q.authGit("update-ref", "-d", "refs/split-gh/applied/fix-foo")
	q.apply(patchFile)
	if q.authGit("rev-parse", "refs/split-gh/applied/fix-foo/bar") != q.authGit("rev-parse", "refs/heads/fix-foo/bar") {
		t.Fatal("marker not set")
	}
}

// Filters run commands the user configured, on the files that
// .gitattributes picks. A command given as a relative path runs from the
// worktree: it is the agent's file.
func TestApplyRunsNoFilters(t *testing.T) {
	q := newQubes(t)
	ran := filepath.Join(q.auth, "ran")
	q.authGit("config", "filter.demo.clean", "./scripts/filter")
	q.authGit("config", "filter.demo.smudge", "./scripts/filter")
	q.authGit("config", "filter.demo.required", "true")
	script := filepath.Join(q.agent, "scripts/filter")
	write(t, script, "#!/bin/sh\necho ran >> "+ran+"\ncat\n")
	if err := os.Chmod(script, 0755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(q.agent, ".gitattributes"), "*.dat filter=demo\n")
	q.git(q.ai, q.agent, "add", ".")
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", "Add a filter")
	q.apply(q.send())
	q.commit("x.dat", "data\n", "Add data")
	q.apply(q.send())
	if b, err := os.ReadFile(ran); err == nil {
		t.Fatalf("apply ran the agent's filter:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(q.auth, "lnd/fix-foo/x.dat")); string(b) != "data\n" {
		t.Fatalf("x.dat is %q", b)
	}
}
