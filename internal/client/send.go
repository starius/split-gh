package client

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"split-gh/internal/patch"
)

// maxAutoCommits stops a guessed base from sending half a repository's
// history, e.g. in a clone that has no remote-tracking branches.
const maxAutoCommits = 100

// Send hands the current branch's commits to gh-auth: it writes them to a
// patch file and moves it with qvm-move, which the user confirms in dom0.
// The user then reviews and applies it in gh-auth.
func Send(args []string) int {
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	base := flags.String("base", "", "commit the series starts after (default: the newest commit that is on a remote)")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: split-gh send [--base COMMIT]")
		return 2
	}
	if err := send(*base); err != nil {
		fmt.Fprintln(os.Stderr, "split-gh send:", err)
		return 1
	}
	return 0
}

func run(dir string, args ...string) (string, error) {
	out, err := patch.Git(dir, nil, args...)
	return strings.TrimSpace(out), err
}

func send(baseRev string) error {
	top, err := run("", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	common, err := run(top, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if filepath.Base(common) != ".git" {
		return fmt.Errorf("%s is not a repository's .git directory", common)
	}
	repo := filepath.Dir(common)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var h patch.Header
	if h.Repo, err = filepath.Rel(home, repo); err != nil || !patch.CleanPath(h.Repo) {
		return fmt.Errorf("%s is not a repository under your home directory, where gh-auth has the same path", repo)
	}
	if h.Worktree, err = filepath.Rel(repo, top); err != nil || h.Worktree == "." {
		return fmt.Errorf("this is the main checkout of %s; work in a worktree (git worktree add), which gh-auth mirrors", repo)
	}
	if h.Branch, err = run(top, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil {
		return fmt.Errorf("HEAD is detached; switch to the branch to send")
	}
	head, err := run(top, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	// The series: the commits after --base, or those that no remote has.
	if baseRev != "" {
		if h.Base, err = run(top, "rev-parse", "--verify", baseRev+"^{commit}"); err != nil {
			return err
		}
	} else {
		out, err := run(top, "rev-list", "--reverse", "--parents", "HEAD", "--not", "--remotes")
		if err != nil {
			return err
		}
		if out == "" {
			return fmt.Errorf("nothing to send: a remote already has every commit on %s", h.Branch)
		}
		first := strings.Fields(strings.SplitN(out, "\n", 2)[0])
		if len(first) != 2 {
			return fmt.Errorf("the first commit to send, %s, is a merge or has no parent; pass --base", first[0][:12])
		}
		h.Base = first[1]
	}
	if err := h.Check(); err != nil {
		return fmt.Errorf("%v (gh-auth accepts letters, digits and ._+-/ in paths and branch names)", err)
	}
	commits, err := patch.Range(top, h.Base, head)
	if err != nil {
		return err
	}
	switch {
	case len(commits) == 0:
		return fmt.Errorf("nothing to send: no commits after %s", h.Base[:12])
	case !patch.Linear(h.Base, commits):
		return fmt.Errorf("the commits after %s are not a straight line; rebase away the merges", h.Base[:12])
	case baseRev == "" && len(commits) > maxAutoCommits:
		return fmt.Errorf("%d commits to send, which looks wrong; pass --base to say where the series starts", len(commits))
	}
	texts, err := patch.Format(top, patch.IDs(commits))
	if err != nil {
		return err
	}
	for i, t := range texts {
		if t == "" {
			return fmt.Errorf("commit %s has no changes; drop it", commits[i].ID[:12])
		}
	}

	data, err := patch.Encode(h, texts)
	if err != nil {
		return err
	}
	if err := roundTrip(top, h.Base, commits, texts); err != nil {
		return err
	}

	name := strings.ReplaceAll(h.Repo+"-"+h.Branch, "/", "-") + "-" + head[:7] + ".patch"
	dir, err := os.MkdirTemp("", "split-gh-send-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, data, 0600); err != nil {
		return err
	}
	move := exec.Command("qvm-move", "--without-progress", file)
	move.Stdout, move.Stderr = os.Stderr, os.Stderr
	if err := move.Run(); err != nil {
		return fmt.Errorf("qvm-move failed or was cancelled: %v", err)
	}
	self, _ := os.Hostname()
	incoming := "~/QubesIncoming/" + self + "/" + name
	fmt.Printf("Sent %d commits on %s as %s.\nIn the authenticated qube, review the change, then apply it:\n  split-gh diff %s\n  split-gh apply %s\n",
		len(commits), h.Base[:12], name, incoming, incoming)
	return nil
}

// roundTrip applies the patches on base in a temporary worktree and checks
// that they rebuild the commits: each one's tree, and its text when formatted
// again. So the patches leave nothing of the commits out, and apply in
// gh-auth will reproduce them.
func roundTrip(dir, base string, commits []patch.Commit, texts []string) error {
	tmp, err := os.MkdirTemp("", "split-gh-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	wt := filepath.Join(tmp, "wt")
	if _, err := run(dir, "worktree", "add", "--quiet", "--detach", wt, base); err != nil {
		return err
	}
	defer run(dir, "worktree", "remove", "--force", wt)
	mbox := filepath.Join(tmp, "series.mbox")
	if err := os.WriteFile(mbox, []byte(strings.Join(texts, "")), 0600); err != nil {
		return err
	}
	env := append(os.Environ(), "GIT_COMMITTER_NAME=split-gh", "GIT_COMMITTER_EMAIL=split-gh@localhost")
	if _, err := patch.Git(wt, env, append(append([]string{}, patch.AmArgs...), "--no-gpg-sign", mbox)...); err != nil {
		return fmt.Errorf("the patch doesn't apply with git am: %v", err)
	}
	replayed, err := patch.Range(wt, base, "HEAD")
	if err != nil {
		return err
	}
	got, err := patch.Format(wt, patch.IDs(replayed))
	if err != nil {
		return err
	}
	if len(got) != len(texts) {
		return fmt.Errorf("git am made %d commits from %d patches; a commit message probably has a line that starts a mail or a diff", len(got), len(texts))
	}
	trees, err := run(dir, append([]string{"rev-parse"}, treesOf(commits)...)...)
	if err != nil {
		return err
	}
	replayedTrees, err := run(dir, append([]string{"rev-parse"}, treesOf(replayed)...)...)
	if err != nil {
		return err
	}
	want, have := strings.Fields(trees), strings.Fields(replayedTrees)
	for i := range texts {
		if have[i] != want[i] {
			return fmt.Errorf("commit %d (%s) has changes that its patch leaves out", i+1, patch.Subject(texts[i]))
		}
		if got[i] != texts[i] {
			return fmt.Errorf("commit %d (%s) changes when applied with git am. A line \"---\" or \"diff ...\" in its message ends the message there; reword it", i+1, patch.Subject(texts[i]))
		}
	}
	return nil
}

// treesOf names the commits' trees, for git rev-parse.
func treesOf(commits []patch.Commit) []string {
	names := make([]string, len(commits))
	for i, c := range commits {
		names[i] = c.ID + "^{tree}"
	}
	return names
}
