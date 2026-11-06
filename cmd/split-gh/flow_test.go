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
	write(t, filepath.Join(q.auth, ".gitconfig"), "[user]\n\tname = User\n\temail = user@auth\n[gpg]\n\tprogram = "+filepath.Join(q.bin, "fake-gpg")+
		"\n[commit]\n\tgpgSign = true\n[init]\n\tdefaultBranch = master\n")
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

	// The first apply fails: no worktree, no branch.
	q.commit("a.txt", "one\n", "Add a")
	patchFile := q.send()
	failed := func(what, want string) {
		t.Helper()
		if out, err := q.splitgh(q.auth, "apply", patchFile); err == nil || !strings.Contains(out, want) || strings.Contains(out, "back failed") {
			t.Fatalf("%s: %v\n%s", what, err, out)
		}
		if _, err := os.Stat(userWT); !os.IsNotExist(err) || q.authGit("for-each-ref", "refs/heads/fix-foo", "refs/split-gh/") != "" {
			t.Fatalf("%s: failed apply left a worktree, branch or marker", what)
		}
	}
	q.authGit("config", "gpg.program", "false")
	failed("signing fails", "gpg failed to sign")
	q.authGit("config", "--unset", "gpg.program")
	// A "From:" line at the top of the message, which git am takes as the
	// author instead of the one you reviewed.
	good, _ := os.ReadFile(patchFile)
	write(t, patchFile, strings.Replace(string(good), "Subject: Add a\n\n", "Subject: Add a\n\nFrom: Evil <evil@ai>\n\n", 1))
	failed("forged author", "came out different")
	write(t, patchFile, string(good))
	q.apply(patchFile)
	tip := q.authGit("rev-parse", "refs/heads/fix-foo")

	// A rewrite whose patch adds a file that is untracked in the worktree.
	// apply moves the branch back to the base, then git am stops: the branch,
	// its marker and the worktree go back.
	write(t, filepath.Join(userWT, "b.txt"), "untracked\n")
	write(t, filepath.Join(q.agent, "b.txt"), "two\n")
	q.git(q.ai, q.agent, "add", "b.txt")
	q.git(q.ai, q.agent, "commit", "--quiet", "--amend", "-m", "Add a and b")
	if out, err := q.splitgh(q.auth, "apply", q.send()); err == nil || !strings.Contains(out, "git am failed") {
		t.Fatalf("apply over an untracked file: %v\n%s", err, out)
	}
	if q.authGit("rev-parse", "refs/heads/fix-foo", "refs/split-gh/applied/fix-foo") != tip+"\n"+tip ||
		q.git(q.auth, userWT, "symbolic-ref", "--short", "HEAD") != "fix-foo" ||
		q.git(q.auth, userWT, "status", "--porcelain", "--untracked-files=no") != "" {
		t.Fatal("failed apply moved the branch or its marker, or left the worktree elsewhere")
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
	for _, name := range []string{"post-checkout", "reference-transaction", "fsmonitor", "applypatch-msg", "pre-applypatch", "post-applypatch", "pre-push"} {
		hook := filepath.Join(q.agent, ".githooks", name)
		write(t, hook, "#!/bin/sh\necho "+name+" >> "+ran+"\n")
		if err := os.Chmod(hook, 0755); err != nil {
			t.Fatal(err)
		}
	}
	q.git(q.ai, q.agent, "add", ".githooks")
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", "Add hooks")
	q.apply(q.send())
	// Now the worktree in gh-auth has the hooks, for the next apply and for
	// the push it prints.
	q.commit("a.txt", "one\n", "Add a")
	line := pushLine(t, q.apply(q.send()))
	if out, err := q.run(q.auth, q.auth, "sh", "-c", line); err != nil {
		t.Fatalf("%s: %v\n%s", line, err, out)
	}
	if b, err := os.ReadFile(ran); err == nil {
		t.Fatalf("apply or its push ran the agent's programs:\n%s", b)
	}
}

// With a relative core.hooksPath, git in the main checkout runs hooks from
// that directory of it: the push apply prints, and any commit or checkout
// there. A worktree in that place would make them the agent's files.
func TestApplyRefusesWorktreeAtHooks(t *testing.T) {
	q := newQubes(t)
	ran := filepath.Join(q.auth, "ran")
	main := filepath.Join(q.ai, "lnd")
	q.git(q.ai, main, "worktree", "move", "fix-foo", ".githooks")
	q.agent = filepath.Join(main, ".githooks")
	hook := filepath.Join(q.agent, "pre-push")
	write(t, hook, "#!/bin/sh\necho pre-push >> "+ran+"\n")
	if err := os.Chmod(hook, 0755); err != nil {
		t.Fatal(err)
	}
	q.git(q.ai, q.agent, "add", "pre-push")
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", "Add a hook")
	patchFile := q.send()
	refused := func(what string) {
		t.Helper()
		out, err := q.splitgh(q.auth, "apply", patchFile)
		if err == nil {
			q.run(q.auth, q.auth, "sh", "-c", pushLine(t, out))
			b, _ := os.ReadFile(ran)
			t.Fatalf("%s: apply put the worktree at the hooks; the push ran:\n%s", what, b)
		}
		if !strings.Contains(out, "hook") || !strings.Contains(out, "worktree") {
			t.Fatalf("%s: %v\n%s", what, err, out)
		}
		if _, err := os.Stat(filepath.Join(q.auth, "lnd/.githooks")); !os.IsNotExist(err) {
			t.Fatalf("%s: apply made the worktree", what)
		}
	}
	// The same directory, spelled in other ways. The symlink's target only
	// exists once apply makes the worktree.
	link := filepath.Join(q.auth, "hooks")
	if err := os.Symlink("lnd/.githooks", link); err != nil {
		t.Fatal(err)
	}
	for _, hooksPath := range []string{".githooks", q.auth + "/lnd/./.githooks", q.auth + "/lnd/.git/../.githooks", link} {
		q.authGit("config", "core.hooksPath", hooksPath)
		refused("core.hooksPath=" + hooksPath)
	}
	// A hook can be a symlink, such as one to the repository's own script.
	q.authGit("config", "--unset", "core.hooksPath")
	if err := os.Symlink("../../.githooks/pre-push", filepath.Join(q.auth, "lnd/.git/hooks/pre-push")); err != nil {
		t.Fatal(err)
	}
	refused("a hook that is a symlink")
	os.Remove(filepath.Join(q.auth, "lnd/.git/hooks/pre-push"))

	// A worktree that apply made before core.hooksPath pointed there: apply
	// refuses every patch while git in the main checkout would run its files.
	q.apply(patchFile)
	q.authGit("config", "core.hooksPath", ".githooks")
	q.git(q.ai, main, "worktree", "add", "--quiet", "-b", "fix-bar", "fix-bar", "origin/master")
	q.agent = filepath.Join(main, "fix-bar")
	q.commit("a.txt", "one\n", "Add a")
	out, err := q.splitgh(q.auth, "apply", q.send())
	if err == nil {
		q.run(q.auth, q.auth, "sh", "-c", pushLine(t, out))
		b, _ := os.ReadFile(ran)
		t.Fatalf("apply printed a push that runs the hooks in a worktree:\n%s", b)
	}
	if !strings.Contains(out, "lnd/.githooks") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
}

// pushLine is the push command in apply's output.
func pushLine(t *testing.T, out string) string {
	t.Helper()
	_, line, ok := strings.Cut(out, "Push it yourself:\n  ")
	line, _, _ = strings.Cut(line, "\n")
	if !ok || line == "" {
		t.Fatalf("no push command:\n%s", out)
	}
	return line
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
		line := pushLine(t, out)
		if !strings.Contains(line, "refs/heads/+master:refs/heads/+master") {
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
	if out, err := q.splitgh(q.auth, "apply", patchFile); err == nil || !strings.Contains(out, "refs/split-gh/applied/fix-foo") || strings.Contains(out, "back failed") {
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

// The configuration can differ in a worktree, as here with an includeIf. git
// worktree add checks out in a git of its own, which reads the new
// worktree's: apply must turn off the filters configured there too.
func TestApplyRunsNoFiltersOfNewWorktrees(t *testing.T) {
	q := newQubes(t)
	ran := filepath.Join(q.auth, "ran")
	include := filepath.Join(q.auth, "worktrees.gitconfig")
	write(t, include, "[filter \"demo\"]\n\tsmudge = ./scripts/filter\n\tclean = ./scripts/filter\n\trequired = true\n")
	q.authGit("config", "includeIf.gitdir:"+filepath.Join(q.auth, "lnd/.git/worktrees")+"/.path", include)
	script := filepath.Join(q.agent, "scripts/filter")
	write(t, script, "#!/bin/sh\necho ran >> "+ran+"\ncat\n")
	if err := os.Chmod(script, 0755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(q.agent, ".gitattributes"), "*.dat filter=demo\n")
	write(t, filepath.Join(q.agent, "x.dat"), "data\n")
	q.git(q.ai, q.agent, "add", ".")
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", "Add a filter")
	q.apply(q.send())
	// The user removes the worktree. The next patch keeps the first commit,
	// so apply makes the worktree again at a commit with the agent's script.
	q.authGit("worktree", "remove", "--force", "fix-foo")
	q.commit("y.txt", "y\n", "Add y")
	q.apply(q.send())
	if b, err := os.ReadFile(ran); err == nil {
		t.Fatalf("apply ran the agent's filter:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(q.auth, "lnd/fix-foo/x.dat")); string(b) != "data\n" {
		t.Fatalf("x.dat is %q", b)
	}
}

// split-gh doesn't sign: git am does, if your git config says to. The harness
// sets commit.gpgSign. git am runs on the branch, as when you run it, so an
// includeIf "onbranch:" applies too.
func TestApplySignsOnlyIfConfigured(t *testing.T) {
	q := newQubes(t)
	signed := func() bool {
		return strings.Contains(q.authGit("cat-file", "commit", "refs/heads/fix-foo"), "\ngpgsig ")
	}
	q.commit("a.txt", "one\n", "Add a")
	q.apply(q.send())
	if !signed() {
		t.Fatal("commit.gpgSign is set, but the commit is not signed")
	}
	q.authGit("config", "commit.gpgSign", "false")
	q.commit("b.txt", "two\n", "Add b")
	q.apply(q.send())
	if signed() {
		t.Fatal("commit.gpgSign is off, but the commit is signed")
	}
	include := filepath.Join(q.auth, "sign.gitconfig")
	write(t, include, "[commit]\n\tgpgSign = true\n")
	q.authGit("config", "includeIf.onbranch:fix-foo.path", include)
	q.commit("c.txt", "three\n", "Add c")
	q.apply(q.send())
	if !signed() {
		t.Fatal("commit.gpgSign is set for the branch, but the commit is not signed")
	}
}

// A submodule's new commit is part of the change. Settings such as
// diff.ignoreSubmodules hide it from format-patch, and then from the patch.
func TestApplyKeepsSubmoduleChanges(t *testing.T) {
	q := newQubes(t)
	q.git(q.ai, filepath.Join(q.ai, "lnd"), "config", "diff.ignoreSubmodules", "all")
	write(t, filepath.Join(q.agent, ".gitmodules"), "[submodule \"sub\"]\n\tpath = sub\n\turl = ./sub\n")
	if err := os.Mkdir(filepath.Join(q.agent, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	q.git(q.ai, q.agent, "add", ".gitmodules")
	q.git(q.ai, q.agent, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",sub")
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", "Add a submodule")
	q.apply(q.send())
	write(t, filepath.Join(q.agent, "a.txt"), "uses the new submodule\n")
	q.git(q.ai, q.agent, "add", "a.txt")
	q.git(q.ai, q.agent, "update-index", "--cacheinfo", "160000,"+strings.Repeat("2", 40)+",sub")
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", "Update the submodule")
	q.apply(q.send())
	for _, rev := range []string{"HEAD~1", "HEAD"} {
		want := q.git(q.ai, q.agent, "rev-parse", rev+"^{tree}")
		if got := q.authGit("rev-parse", "refs/heads/fix-foo"+strings.TrimPrefix(rev, "HEAD")+"^{tree}"); got != want {
			t.Fatalf("%s: the tree in gh-auth differs from the agent's:\n%s", rev, q.authGit("ls-tree", "refs/heads/fix-foo"))
		}
	}
}

// With submodule.recurse, git switch also checks out submodules, by running
// git in them. apply's switches must not: they would act on the submodule
// commit that the agent picked, in a worktree whose submodules aren't set up.
func TestApplyStaysOutOfSubmodules(t *testing.T) {
	q := newQubes(t)
	sub := filepath.Join(t.TempDir(), "sub")
	q.git(q.auth, "", "init", "--quiet", sub)
	q.git(q.auth, sub, "commit", "--quiet", "--allow-empty", "-m", "One")
	q.git(q.auth, sub, "commit", "--quiet", "--allow-empty", "-m", "Two")
	two := q.git(q.auth, sub, "rev-parse", "HEAD")
	q.authGit("-c", "protocol.file.allow=always", "submodule", "--quiet", "add", sub, "sub")
	q.git(q.auth, filepath.Join(q.auth, "lnd/sub"), "checkout", "--quiet", "HEAD~1")
	q.authGit("add", "sub")
	q.authGit("commit", "--quiet", "-m", "Add a submodule")
	q.authGit("push", "--quiet", "origin", "master")
	q.authGit("config", "submodule.recurse", "true")
	main := filepath.Join(q.ai, "lnd")
	q.git(q.ai, main, "fetch", "--quiet", "origin")
	q.git(q.ai, q.agent, "reset", "--quiet", "--hard", "origin/master")
	q.git(q.ai, q.agent, "update-index", "--cacheinfo", "160000,"+two+",sub")
	q.git(q.ai, q.agent, "commit", "--quiet", "-m", "Update the submodule")
	q.apply(q.send())
	// A rewrite: apply switches the worktree back to the base, where the
	// submodule is at its first commit.
	q.git(q.ai, q.agent, "commit", "--quiet", "--amend", "-m", "Update the submodule, reworded")
	q.apply(q.send())
	if got := q.authGit("rev-parse", "refs/heads/fix-foo:sub"); got != two {
		t.Fatalf("sub is at %s", got)
	}
}

// When apply goes back to an older commit, git would overwrite an ignored
// file of the user's that the older commit tracks.
func TestApplyKeepsIgnoredFiles(t *testing.T) {
	q := newQubes(t)
	q.commit("cache.dat", "tracked\n", "Track the cache")
	q.apply(q.send())
	q.git(q.ai, q.agent, "rm", "--quiet", "cache.dat")
	q.commit(".gitignore", "cache.dat\n", "Ignore the cache")
	q.apply(q.send())
	mine := filepath.Join(q.auth, "lnd/fix-foo/cache.dat")
	write(t, mine, "mine\n")
	// A rewrite of the last commit: apply starts from the first one, which
	// tracks cache.dat.
	q.git(q.ai, q.agent, "commit", "--quiet", "--amend", "-m", "Ignore the cache, reworded")
	tip := q.authGit("rev-parse", "refs/heads/fix-foo")
	if out, err := q.splitgh(q.auth, "apply", q.send()); err == nil || !strings.Contains(out, "cache.dat") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(mine); string(b) != "mine\n" {
		t.Fatalf("the user's file is now %q", b)
	}
	if q.authGit("rev-parse", "refs/heads/fix-foo") != tip {
		t.Fatal("the branch moved")
	}
}
