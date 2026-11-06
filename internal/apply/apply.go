// Package apply is what the user runs in gh-auth to review and take a patch
// file from ai: `split-gh diff` and `split-gh apply`. Nothing in ai can start
// it; the patch only arrives through qvm-move, which the user confirms. apply
// never signs: it prints the git am that adds the commits, which the user
// runs, and which signs them if the user's git config says to.
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

// apply keeps two files for each branch in the repository's git directory:
//   - split-gh/<branch>.patch, the last patch file it prepared the branch for.
//     A commit on the branch that isn't in it, such as one of yours, isn't
//     the agent's, so apply drops it only with --force.
//   - split-gh/<branch>.mbox, the patches that the printed git am adds.
const stateDir = "split-gh"

type plan struct {
	file      string
	qube      string // the qube that sent the patch
	h         patch.Header
	patches   []string
	repo      string // the repository's main checkout
	worktree  string
	exists    bool     // the worktree exists
	worktrees []string // the patch's worktree and the other linked worktrees, symlinks resolved
	gitDir    string   // the repository's git directory, where apply keeps its files
	tip       string   // the branch's commit, "" if there is no branch
	old       []patch.Commit
	oldTexts  []string
	keep      int      // leading commits of the branch that equal the patch's
	lost      []string // commits on the branch whose change the patch doesn't have
	foreign   bool     // some of them aren't in the last patch apply prepared
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
	} else if !fi.Mode().IsRegular() || fi.Size() > patch.MaxFile {
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
	if p.gitDir, err = revParse(p.repo, "--path-format=absolute", "--git-common-dir"); err != nil {
		return nil, err
	}
	p.tip = patch.Ref(p.repo, "refs/heads/"+p.h.Branch)
	p.worktree = filepath.Join(p.repo, p.h.Worktree)
	if err := p.checkHooks(real); err != nil {
		return nil, err
	}
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
	inPatch, prepared := keys(p.patches), map[string]bool{}
	if data, err := os.ReadFile(p.state("patch")); err == nil {
		if _, texts, err := patch.Parse(data); err == nil {
			prepared = keys(texts)
		}
	}
	for i := p.keep; i < len(p.old); i++ {
		key := patch.Key(p.oldTexts[i])
		if p.oldTexts[i] == "" || !inPatch[key] {
			line, _ := patch.Git(p.repo, nil, "log", "-1", "--format=%h %s", p.old[i].ID)
			p.lost = append(p.lost, patch.Visible(strings.TrimSpace(line)))
			p.foreign = p.foreign || p.oldTexts[i] == "" || !prepared[key]
		}
	}
	return p, nil
}

func keys(texts []string) map[string]bool {
	m := map[string]bool{}
	for _, t := range texts {
		m[patch.Key(t)] = true
	}
	return m
}

// state is the path of one of the files apply keeps for the branch.
func (p *plan) state(ext string) string {
	return filepath.Join(p.gitDir, stateDir, p.h.Branch+"."+ext)
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

// checkHooks refuses to apply while git in the main checkout would run a hook
// from a worktree: this patch's, or one that apply made for an earlier patch.
// A relative core.hooksPath such as .githooks names a directory of the main
// checkout, where a worktree of that name would put the agent's files; so can
// an absolute path or a symlink, and a hook can be a symlink too. The printed
// push, or a commit in the main checkout, would then run the agent's files.
func (p *plan) checkHooks(real string) error {
	if err := p.listWorktrees(real); err != nil {
		return err
	}
	// Not patch.Git, which sets core.hooksPath.
	cmd := exec.Command("git", "rev-parse", "--git-path", "hooks")
	cmd.Dir = p.repo
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git rev-parse --git-path hooks: %v", err)
	}
	hooks := strings.TrimSuffix(string(out), "\n")
	if !filepath.IsAbs(hooks) {
		hooks = p.repo + "/" + hooks // not filepath.Join, which drops ".." before symlinks are followed
	}
	check := func(path, what string) error {
		wt, err := p.worktreeOn(path)
		if err != nil || wt == "" {
			return err
		}
		return fmt.Errorf("git in %s %s, which leads into the worktree %s: git there, and the push that apply prints, would run the agent's files",
			p.repo, what, wt)
	}
	if err := check(hooks, "looks for hooks in "+hooks+" (core.hooksPath)"); err != nil {
		return err
	}
	entries, _ := os.ReadDir(hooks)
	for _, e := range entries {
		if err := check(hooks+"/"+e.Name(), "runs the hook "+hooks+"/"+e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// listWorktrees sets p.worktrees to the directories that hold the agent's
// files: the patch's worktree, which may not exist yet, and the repository's
// other linked worktrees, which apply may have made for earlier patches.
func (p *plan) listWorktrees(real string) error {
	p.worktrees = []string{filepath.Join(real, p.h.Worktree)}
	out, err := patch.Git(p.repo, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	main := true
	for _, line := range strings.Split(out, "\n") {
		if wt, ok := strings.CutPrefix(line, "worktree "); ok {
			if r, err := filepath.EvalSymlinks(wt); err == nil && !main {
				p.worktrees = append(p.worktrees, r)
			}
			main = false
		}
	}
	return nil
}

// worktreeOn returns the worktree that the kernel goes into on its way to
// path, an absolute path, or "" if it doesn't.
func (p *plan) worktreeOn(path string) (string, error) {
	dirs, err := resolve(path)
	if err != nil {
		return "", err
	}
	for _, dir := range dirs {
		for _, wt := range p.worktrees {
			if dir == wt || strings.HasPrefix(dir, wt+"/") {
				return wt, nil
			}
		}
	}
	return "", nil
}

// resolve follows path, an absolute path, as the kernel would, and returns
// each place it goes through, ending with path's own, with symlinks followed.
// What doesn't exist yet is taken as written: it may exist later.
func resolve(path string) ([]string, error) {
	var dirs []string
	dir, parts := "/", strings.Split(path, "/")
	for links := 0; len(parts) > 0; {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			dir = filepath.Dir(dir)
		default:
			next := filepath.Join(dir, part)
			if target, err := os.Readlink(next); err == nil {
				if links++; links > 40 {
					return nil, fmt.Errorf("%s: too many symlinks", path)
				}
				if filepath.IsAbs(target) {
					dir = "/"
				}
				parts = append(strings.Split(target, "/"), parts...)
				continue
			}
			dir = next
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func revParse(dir string, args ...string) (string, error) {
	out, err := patch.Git(dir, nil, append([]string{"rev-parse"}, args...)...)
	return strings.TrimSpace(out), err
}

// needsForce reports whether applying would drop commits that aren't in the
// last patch apply prepared the branch for, e.g. the user's own in gh-auth.
func (p *plan) needsForce() bool {
	return p.foreign
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
	fmt.Fprintf(w, "Apply     keeps %d of the branch's commits; the git am it prints adds %d\n", p.keep, len(p.patches)-p.keep)
	if len(p.lost) > 0 {
		what := "the patch replaces"
		if p.needsForce() {
			what = "the patch drops, not all from the agent; apply needs --force"
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

// Apply prepares the same-path worktree and branch for the patch file's
// series, and prints the git am that adds its commits.
func Apply(args []string) int {
	flags := flag.NewFlagSet("apply", flag.ContinueOnError)
	force := flags.Bool("force", false, "drop commits on the branch that aren't in the last patch apply prepared")
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

// apply puts the worktree on the branch at the last commit it keeps, and
// writes the rest of the patches to an mbox for the git am that it prints.
func (p *plan) apply(force bool) error {
	if p.needsForce() && !force {
		return fmt.Errorf("%s has commits that aren't in the last patch apply prepared, such as your own, and this patch drops them:\n  %s\n"+
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
		// one of them. Ignored ones too: by default, git switch would
		// overwrite an ignored file that the commit it switches to tracks.
		if out, err := patch.Git(p.worktree, nil, "status", "--porcelain", "--untracked-files=no"); err != nil || out != "" {
			return fmt.Errorf("%s has uncommitted changes; commit or stash them first", p.worktree)
		}
		if _, err := patch.Git(p.worktree, nil, "switch", "--quiet", "--no-overwrite-ignore", "--no-recurse-submodules", "--detach", start); err != nil {
			return err
		}
	} else {
		// git worktree add would check out in a git of its own, which reads
		// the new worktree's configuration: that can name filters that the
		// main checkout's doesn't, so patch.Command wouldn't turn them off.
		// The checkout runs in the new worktree instead.
		if _, err := patch.Git(p.repo, nil, "worktree", "add", "--quiet", "--no-checkout", "--detach", p.worktree, start); err != nil {
			return err
		}
		if _, err := patch.Git(p.worktree, nil, "reset", "--quiet", "--hard", "--no-recurse-submodules"); err != nil {
			patch.Git(p.repo, nil, "worktree", "remove", "--force", p.worktree)
			return err
		}
	}
	err := p.check()
	if err == nil {
		err = p.setBranch(p.tip, start)
	}
	if err == nil {
		// This also takes the worktree back from check's commits to start.
		if _, err = patch.Git(p.worktree, nil, "switch", "--quiet", "--no-overwrite-ignore", "--no-recurse-submodules", p.h.Branch); err != nil {
			if undo := p.setBranch(start, p.tip); undo != nil {
				err = fmt.Errorf("%v\nMoving %s back failed too: %v", err, p.h.Branch, undo)
			}
		}
	}
	if err != nil {
		// Put the worktree back. The branch hasn't moved.
		if p.exists {
			patch.Git(p.worktree, nil, "switch", "--quiet", "--no-overwrite-ignore", "--no-recurse-submodules", p.h.Branch)
		} else {
			patch.Git(p.repo, nil, "worktree", "remove", "--force", p.worktree)
		}
		return err
	}
	if err := p.save(); err != nil {
		return fmt.Errorf("%s is ready at %s, but apply couldn't write its files: %v\nRerun apply", p.h.Branch, start[:12], err)
	}
	rest := len(p.patches) - p.keep
	fmt.Printf("%s is at %s, in %s, with %d of the patch's %d commits.\n", p.h.Branch, start[:12], p.worktree, p.keep, len(p.patches))
	// From the main checkout: in the worktree, with a relative
	// core.hooksPath, git push would run the agent's pre-push hook.
	push := fmt.Sprintf("  cd %s && %s\n", quote(p.repo), p.pushCommand(start))
	if rest == 0 {
		fmt.Print("Push it yourself:\n" + push)
	} else {
		am, err := patch.Command(p.worktree, append(append([]string{}, patch.AmArgs...), p.state("mbox"))...)
		if err != nil {
			return err
		}
		what := fmt.Sprintf("the other %d commits", rest)
		if rest == 1 {
			what = "the other commit"
		}
		fmt.Printf("Add %s with git am, which signs if your git config says to (commit.gpgSign).\n"+
			"It runs no hooks or filters, as they could be the agent's files:\n  cd %s && %s\nThen push:\n%s",
			what, quote(p.worktree), shellJoin(am.Args), push)
	}
	if err := os.Remove(p.file); err == nil {
		fmt.Printf("Removed %s.\n", p.file)
	}
	return nil
}

// check runs git am, without signing, on the patches after the kept ones,
// in the worktree at the last kept commit. It requires one commit per patch,
// and that each of the branch's commits then formats to exactly its patch:
// so the git am that apply prints gives the commits you reviewed. It leaves
// the worktree at the commits it made, which nothing refers to.
func (p *plan) check() error {
	if p.keep == len(p.patches) {
		return nil
	}
	f, err := os.CreateTemp("", "split-gh-*.mbox")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(strings.Join(p.patches[p.keep:], ""))
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err != nil {
		return err
	}
	cmd, err := patch.Command(p.worktree, append(append([]string{}, patch.AmArgs...), "--quiet", "--no-gpg-sign", f.Name())...)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		patch.Git(p.worktree, nil, "am", "--abort")
		return fmt.Errorf("git am failed:\n%s", patch.Visible(string(out)))
	}
	commits, err := patch.Range(p.worktree, p.h.Base, "HEAD")
	if err != nil {
		return err
	}
	if !patch.Linear(p.h.Base, commits) || len(commits) != len(p.patches) {
		return errors.New("git am did not make one commit per patch")
	}
	texts, err := patch.Format(p.worktree, patch.IDs(commits))
	if err != nil {
		return err
	}
	for i := range texts {
		if patch.Key(texts[i]) != patch.Key(p.patches[i]) {
			return fmt.Errorf("patch %d (%s) came out different after git am", i+1, patch.Subject(p.patches[i]))
		}
	}
	return nil
}

// save writes the files apply keeps for the branch, once the branch is
// ready: the mbox of the patches that git am adds, or no mbox if there are
// none, and the patch file.
func (p *plan) save() error {
	if err := os.MkdirAll(filepath.Dir(p.state("patch")), 0700); err != nil {
		return err
	}
	if p.keep == len(p.patches) {
		if err := os.Remove(p.state("mbox")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else if err := os.WriteFile(p.state("mbox"), []byte(strings.Join(p.patches[p.keep:], "")), 0600); err != nil {
		return err
	}
	data, err := patch.Encode(p.h, p.patches)
	if err != nil {
		return err
	}
	return os.WriteFile(p.state("patch"), data, 0600)
}

// setBranch moves the branch from from to to, if nobody moved it since apply
// read it. "" is a branch that doesn't exist.
func (p *plan) setBranch(from, to string) error {
	zero := strings.Repeat("0", len(p.h.Base))
	if from == "" {
		from = zero
	}
	args := []string{"update-ref", "-m", "split-gh apply", "refs/heads/" + p.h.Branch, to, from}
	if to == "" {
		args = []string{"update-ref", "-m", "split-gh apply", "-d", "refs/heads/" + p.h.Branch, from}
	}
	_, err := patch.Git(p.repo, nil, args...)
	return err
}

// shellJoin writes a command line for a shell.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quote(a)
	}
	return strings.Join(quoted, " ")
}

// quote quotes s for a shell, unless it needs none.
func quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./_-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pushCommand is the push the user runs: a plain push when the remote branch
// is behind start, else one that only overwrites what the user has fetched.
// start is the branch now; the commits that git am adds go on it, so the
// remote branch is behind the new tip exactly when it is behind start.
// It names the branch by its full ref on both sides. Short, git push would
// read "+main" as a forced push of main, and could take "heads/main" for
// refs/heads/main.
func (p *plan) pushCommand(start string) string {
	remote := "origin"
	for _, key := range []string{"branch." + p.h.Branch + ".pushRemote", "remote.pushDefault", "branch." + p.h.Branch + ".remote"} {
		if out, err := patch.Git(p.repo, nil, "config", "--get", key); err == nil && strings.TrimSpace(out) != "" {
			remote = strings.TrimSpace(out)
			break
		}
	}
	ref := "refs/heads/" + p.h.Branch
	if r := patch.Ref(p.repo, "refs/remotes/"+remote+"/"+p.h.Branch); r != "" {
		if _, err := patch.Git(p.repo, nil, "merge-base", "--is-ancestor", r, start); err != nil {
			return fmt.Sprintf("git push --force-with-lease=%s:%s %s %s:%s", ref, r, quote(remote), ref, ref)
		}
	}
	return fmt.Sprintf("git push %s %s:%s", quote(remote), ref, ref)
}
