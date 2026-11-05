// Package apply is what the user runs in gh-auth to review and take a patch
// file from ai: `split-gh diff` and `split-gh apply`. Nothing in ai can start
// it; the patch only arrives through qvm-move, which the user confirms.
package apply

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"split-gh/internal/config"
	"split-gh/internal/patch"
)

// appliedRef records the tip apply last set on each branch. Commits above it
// were put there by someone else, so apply won't drop them without --force.
const appliedRef = "refs/split-gh/applied/"

const maxPatchBytes = 64 << 20

type plan struct {
	file     string
	qube     string // the qube that sent the patch
	h        patch.Header
	patches  []string
	repo     string // the repository's main checkout
	worktree string
	exists   bool   // the worktree exists
	tip      string // the branch's commit, "" if there is no branch
	applied  string // the tip apply last set, "" if none
	old      []patch.Commit
	oldTexts []string
	keep     int      // leading commits of the branch that equal the patch's
	lost     []string // commits on the branch whose change the patch doesn't have
}

func load(file string) (*plan, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(filepath.Join(home, "QubesIncoming"), abs)
	parts := strings.Split(rel, "/")
	if err != nil || len(parts) != 2 || parts[0] == ".." {
		return nil, fmt.Errorf("%s is not in ~/QubesIncoming/<qube>/, so the qube that sent it is unknown", file)
	}
	p := &plan{file: abs, qube: parts[0]}
	cfg, err := config.Load(home)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if fi, err := os.Lstat(abs); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() || fi.Size() > maxPatchBytes {
		return nil, fmt.Errorf("%s is not a regular file under 64 MiB", file)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if p.h, p.patches, err = patch.Parse(data); err != nil {
		return nil, err
	}
	if !cfg.Qubes[p.qube].AllowsLocal(p.h.Repo) {
		return nil, fmt.Errorf("~/%s is not in local_repos for %s", p.h.Repo, p.qube)
	}
	p.repo = filepath.Join(home, p.h.Repo)
	real, err := filepath.EvalSymlinks(p.repo)
	if err != nil {
		return nil, err
	}
	if top, err := revParse(p.repo, "--show-toplevel"); err != nil || top != real {
		return nil, fmt.Errorf("%s is not the main checkout of a git repository", p.repo)
	}
	if out, err := patch.Git(p.repo, nil, "check-ref-format", "--branch", p.h.Branch); err != nil || strings.TrimSpace(out) != p.h.Branch {
		return nil, fmt.Errorf("invalid branch name %q", p.h.Branch)
	}
	if patch.Ref(p.repo, p.h.Base) != p.h.Base {
		return nil, fmt.Errorf("base commit %s is not in %s; fetch it (git fetch --all) and rerun", p.h.Base[:12], p.repo)
	}
	p.tip = patch.Ref(p.repo, "refs/heads/"+p.h.Branch)
	p.applied = patch.Ref(p.repo, appliedRef+p.h.Branch)
	p.worktree = filepath.Join(p.repo, p.h.Worktree)
	if err := p.checkWorktree(real); err != nil {
		return nil, err
	}
	if p.tip != "" {
		if p.old, err = patch.Range(p.repo, p.h.Base, p.tip); err != nil {
			return nil, err
		}
		if p.oldTexts, err = patch.Format(p.repo, patch.IDs(p.old)); err != nil {
			return nil, err
		}
	}
	// Keep the leading commits that the branch already has unchanged, so a
	// series that only grew stays a fast-forward.
	for p.keep < len(p.old) && p.keep < len(p.patches) && patch.Linear(p.h.Base, p.old[:p.keep+1]) &&
		p.oldTexts[p.keep] != "" && patch.Key(p.oldTexts[p.keep]) == patch.Key(p.patches[p.keep]) {
		p.keep++
	}
	inPatch := map[string]bool{}
	for _, t := range p.patches {
		inPatch[patch.Key(t)] = true
	}
	for i := p.keep; i < len(p.old); i++ {
		if p.oldTexts[i] == "" || !inPatch[patch.Key(p.oldTexts[i])] {
			line, _ := patch.Git(p.repo, nil, "log", "-1", "--format=%h %s", p.old[i].ID)
			p.lost = append(p.lost, patch.Visible(strings.TrimSpace(line)))
		}
	}
	return p, nil
}

// checkWorktree requires the target to be a worktree of the repository on the
// patch's branch, or not to exist. Nothing on the way may be a symlink.
func (p *plan) checkWorktree(real string) error {
	p.exists = true
	path := p.repo
	for _, c := range strings.Split(p.h.Worktree, "/") {
		path = filepath.Join(path, c)
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			p.exists = false
			break
		} else if err != nil {
			return err
		} else if !fi.IsDir() {
			return fmt.Errorf("%s is a symlink or a file", path)
		}
	}
	if p.exists {
		top, err := revParse(p.worktree, "--show-toplevel")
		common, err2 := revParse(p.worktree, "--path-format=absolute", "--git-common-dir")
		if err != nil || err2 != nil || top != filepath.Join(real, p.h.Worktree) || common != filepath.Join(real, ".git") {
			return fmt.Errorf("%s exists but is not a worktree of %s", p.worktree, p.repo)
		}
		head, _ := patch.Git(p.worktree, nil, "symbolic-ref", "--quiet", "HEAD")
		if strings.TrimSpace(head) != "refs/heads/"+p.h.Branch {
			return fmt.Errorf("%s is not on branch %s", p.worktree, p.h.Branch)
		}
		return nil
	}
	out, err := patch.Git(p.repo, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	where := ""
	for _, line := range strings.Split(out, "\n") {
		if s, ok := strings.CutPrefix(line, "worktree "); ok {
			where = s
		} else if line == "branch refs/heads/"+p.h.Branch {
			return fmt.Errorf("branch %s is checked out in %s, not in %s", p.h.Branch, where, p.worktree)
		}
	}
	return nil
}

func revParse(dir string, args ...string) (string, error) {
	out, err := patch.Git(dir, nil, append([]string{"rev-parse"}, args...)...)
	return strings.TrimSpace(out), err
}

// needsForce reports whether applying would drop commits that apply didn't
// put on the branch, e.g. the user's own edits in gh-auth.
func (p *plan) needsForce() bool {
	return len(p.lost) > 0 && p.tip != p.applied
}

func (p *plan) describe(w io.Writer) {
	fmt.Fprintf(w, "Patch     %s (from %s)\n", p.file, p.qube)
	state := "exists"
	if !p.exists {
		state = "will be created"
	}
	fmt.Fprintf(w, "Worktree  %s (%s)\n", p.worktree, state)
	if p.tip == "" {
		fmt.Fprintf(w, "Branch    %s (new)\n", p.h.Branch)
	} else {
		fmt.Fprintf(w, "Branch    %s, now at %s\n", p.h.Branch, p.tip[:12])
	}
	base, _ := patch.Git(p.repo, nil, "log", "-1", "--format=%h %s", p.h.Base)
	fmt.Fprintf(w, "Base      %s\n", patch.Visible(strings.TrimSpace(base)))
	fmt.Fprintf(w, "Apply     keeps %d of the branch's commits, then signs and adds %d\n", p.keep, len(p.patches)-p.keep)
	if len(p.lost) > 0 {
		what := "the patch replaces"
		if p.needsForce() {
			what = "split-gh didn't put there; apply needs --force to drop"
		}
		fmt.Fprintf(w, "          commits %s:\n", what)
		for _, l := range p.lost {
			fmt.Fprintf(w, "            %s\n", l)
		}
	}
	fmt.Fprintln(w)
}

// Diff shows the branch as patches next to the patch file.
func Diff(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: split-gh diff PATCH")
		return 2
	}
	p, err := load(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "split-gh diff:", err)
		return 1
	}
	var page bytes.Buffer
	p.describe(&page)
	if err := unified(&page, p.h.Branch+" in gh-auth", filepath.Base(p.file), p.oldTexts, p.patches); err != nil {
		fmt.Fprintln(os.Stderr, "split-gh diff:", err)
		return 1
	}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		less := exec.Command("less", "-R")
		less.Stdin, less.Stdout, less.Stderr = &page, os.Stdout, os.Stderr
		if less.Run() == nil {
			return 0
		}
	}
	os.Stdout.Write(page.Bytes())
	return 0
}

// unified writes `diff -u --color=always` of two series to w.
func unified(w io.Writer, oldLabel, newLabel string, old, new []string) error {
	dir, err := os.MkdirTemp("", "split-gh-diff-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var files []string
	for i, series := range [][]string{old, new} {
		var b strings.Builder
		for _, t := range series {
			b.WriteString(patch.Visible(patch.Key(t)))
		}
		f := filepath.Join(dir, fmt.Sprint(i))
		if err := os.WriteFile(f, []byte(b.String()), 0600); err != nil {
			return err
		}
		files = append(files, f)
	}
	cmd := exec.Command("diff", "-u", "--color=always", "--label", oldLabel, "--label", newLabel, files[0], files[1])
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		if x, ok := err.(*exec.ExitError); !ok || x.ExitCode() != 1 {
			return fmt.Errorf("diff: %v", err)
		}
	}
	return nil
}

// Apply updates the same-path worktree to the patch file's series.
func Apply(args []string) int {
	flags := flag.NewFlagSet("apply", flag.ContinueOnError)
	force := flags.Bool("force", false, "drop commits on the branch that split-gh didn't put there")
	if flags.Parse(args) != nil || flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: split-gh apply [--force] PATCH")
		return 2
	}
	p, err := load(flags.Arg(0))
	if err == nil {
		err = p.apply(*force)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "split-gh apply:", err)
		return 1
	}
	return 0
}

func (p *plan) apply(force bool) error {
	if p.needsForce() && !force {
		return fmt.Errorf("%s has commits that split-gh didn't put there, and this patch drops them:\n  %s\n"+
			"The agent sees them once you push them: push, let the agent build on them and send again.\n"+
			"Or rerun with --force to drop them",
			p.h.Branch, strings.Join(p.lost, "\n  "))
	}
	start := p.h.Base
	if p.keep > 0 {
		start = p.old[p.keep-1].ID
	}
	if p.exists {
		// Untracked files stay where they are; git am stops if a patch adds
		// one of them.
		if out, err := patch.Git(p.worktree, nil, "status", "--porcelain", "--untracked-files=no"); err != nil || out != "" {
			return fmt.Errorf("%s has uncommitted changes; commit or stash them first", p.worktree)
		}
		if _, err := patch.Git(p.worktree, nil, "switch", "--quiet", "--detach", start); err != nil {
			return err
		}
	} else if _, err := patch.Git(p.repo, nil, "worktree", "add", "--quiet", "--detach", p.worktree, start); err != nil {
		return err
	}
	tip, err := p.commit()
	if err == nil {
		err = p.setRefs([2]string{p.tip, p.applied}, [2]string{tip, tip})
	}
	if err == nil {
		// HEAD is at tip already, so this only attaches it to the branch.
		if _, err = patch.Git(p.worktree, nil, "switch", "--quiet", p.h.Branch); err != nil {
			if undo := p.setRefs([2]string{tip, tip}, [2]string{p.tip, p.applied}); undo != nil {
				err = fmt.Errorf("%v\nMoving %s back failed too: %v", err, p.h.Branch, undo)
			}
		}
	}
	if err != nil {
		// Put the worktree back. The branch hasn't moved.
		if p.exists {
			patch.Git(p.worktree, nil, "switch", "--quiet", p.h.Branch)
		} else {
			patch.Git(p.repo, nil, "worktree", "remove", "--force", p.worktree)
		}
		return err
	}
	fmt.Printf("%s is now at %s: %d commits on %s, %d kept and %d signed and added.\n",
		p.h.Branch, tip[:12], len(p.patches), p.h.Base[:12], p.keep, len(p.patches)-p.keep)
	fmt.Printf("Push it yourself:\n  cd %s && %s\n", p.worktree, p.pushCommand(tip))
	if err := os.Remove(p.file); err == nil {
		fmt.Printf("Removed %s.\n", p.file)
	}
	return nil
}

// setRefs moves the branch and its applied marker from the values in from to
// the values in to, in one transaction: both move or neither does, and only
// if neither changed since apply read them. "" is a ref that doesn't exist.
func (p *plan) setRefs(from, to [2]string) error {
	var script strings.Builder
	for i, ref := range []string{"refs/heads/" + p.h.Branch, appliedRef + p.h.Branch} {
		old := from[i]
		if old == "" {
			old = strings.Repeat("0", len(p.h.Base))
		}
		if to[i] == "" {
			fmt.Fprintf(&script, "delete %s %s\n", ref, old)
		} else {
			fmt.Fprintf(&script, "update %s %s %s\n", ref, to[i], old)
		}
	}
	cmd, err := patch.Command(p.repo, "update-ref", "-m", "split-gh apply", "--stdin")
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(script.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git update-ref: %s", patch.Visible(strings.TrimSpace(string(out))))
	}
	return nil
}

// commit applies the patches after the kept commits, signing each new commit,
// and checks that the branch now formats to exactly the patch file.
func (p *plan) commit() (string, error) {
	if rest := p.patches[p.keep:]; len(rest) > 0 {
		f, err := os.CreateTemp("", "split-gh-*.mbox")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		_, err = f.WriteString(strings.Join(rest, ""))
		if err2 := f.Close(); err == nil {
			err = err2
		}
		if err != nil {
			return "", err
		}
		cmd, err := patch.Command(p.worktree, append(append([]string{}, patch.AmArgs...), "--gpg-sign", f.Name())...)
		if err != nil {
			return "", err
		}
		cmd.Stdin = os.Stdin
		if out, err := cmd.CombinedOutput(); err != nil {
			patch.Git(p.worktree, nil, "am", "--abort")
			return "", fmt.Errorf("git am failed:\n%s", patch.Visible(string(out)))
		}
	}
	tip := patch.Ref(p.worktree, "HEAD")
	commits, err := patch.Range(p.worktree, p.h.Base, tip)
	if err != nil {
		return "", err
	}
	if !patch.Linear(p.h.Base, commits) || len(commits) != len(p.patches) {
		return "", errors.New("git am did not make one commit per patch")
	}
	texts, err := patch.Format(p.worktree, patch.IDs(commits))
	if err != nil {
		return "", err
	}
	for i := range texts {
		if patch.Key(texts[i]) != patch.Key(p.patches[i]) {
			return "", fmt.Errorf("patch %d (%s) came out different after git am", i+1, patch.Subject(p.patches[i]))
		}
	}
	for _, c := range commits[p.keep:] {
		if obj, err := patch.Git(p.worktree, nil, "cat-file", "commit", c.ID); err != nil || !signed(obj) {
			return "", fmt.Errorf("commit %s is not signed; check your signing setup", c.ID[:12])
		}
	}
	return tip, nil
}

func signed(commit string) bool {
	head, _, _ := strings.Cut(commit, "\n\n")
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "gpgsig ") || strings.HasPrefix(line, "gpgsig-sha256 ") {
			return true
		}
	}
	return false
}

// pushCommand is the push the user runs: a plain push when the remote branch
// is behind tip, else one that only overwrites what the user has fetched.
// It names the branch by its full ref on both sides. Short, git push would
// read "+main" as a forced push of main, and could take "heads/main" for
// refs/heads/main.
func (p *plan) pushCommand(tip string) string {
	remote := "origin"
	for _, key := range []string{"branch." + p.h.Branch + ".pushRemote", "remote.pushDefault", "branch." + p.h.Branch + ".remote"} {
		if out, err := patch.Git(p.repo, nil, "config", "--get", key); err == nil && strings.TrimSpace(out) != "" {
			remote = strings.TrimSpace(out)
			break
		}
	}
	ref := "refs/heads/" + p.h.Branch
	if r := patch.Ref(p.repo, "refs/remotes/"+remote+"/"+p.h.Branch); r != "" {
		if _, err := patch.Git(p.repo, nil, "merge-base", "--is-ancestor", r, tip); err != nil {
			return fmt.Sprintf("git push --force-with-lease=%s:%s %s %s:%s", ref, r, remote, ref, ref)
		}
	}
	return fmt.Sprintf("git push %s %s:%s", remote, ref, ref)
}
