// Package patch is the patch file that carries commits from ai to gh-auth:
// a short header, then `git format-patch` output, one patch per commit.
//
//	split-gh-patch 1
//	repo: lnd
//	worktree: fix-foo
//	branch: fix-foo
//	base: 2f149331...
//
//	From 0000000000000000000000000000000000000000 Mon Sep 17 00:00:00 2001
//	From: ...
//
// Both qubes format commits with the same pinned options, so the same commit
// gives the same text in either qube. That is what lets gh-auth compare its
// branch with a patch, and check that `git am` reproduced the patch exactly.
package patch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const magic = "split-gh-patch 1"

// Separator starts every patch; format-patch --zero-commit prints it.
const Separator = "From 0000000000000000000000000000000000000000 Mon Sep 17 00:00:00 2001\n"

// Header says where in gh-auth a patch belongs.
type Header struct {
	Repo     string // repository, relative to the home directory
	Worktree string // worktree, relative to the repository
	Branch   string
	Base     string // full ID of the commit the series applies on
}

// Paths and branch names are limited to characters that need no quoting in
// a shell and look the same in any terminal.
var (
	componentRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	commitRE    = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// CleanPath reports whether p is a relative path of plain components.
func CleanPath(p string) bool {
	if p == "" || len(p) > 200 {
		return false
	}
	for _, c := range strings.Split(p, "/") {
		if c == "." || c == ".." || !componentRE.MatchString(c) {
			return false
		}
	}
	return true
}

// Check validates the header's syntax. gh-auth also checks what it names.
func (h Header) Check() error {
	switch {
	case !CleanPath(h.Repo):
		return fmt.Errorf("invalid repository path %q", h.Repo)
	case !CleanPath(h.Worktree) || strings.EqualFold(strings.Split(h.Worktree, "/")[0], ".git"):
		return fmt.Errorf("invalid worktree path %q", h.Worktree)
	case !CleanPath(h.Branch) || strings.HasPrefix(h.Branch, "-"):
		return fmt.Errorf("invalid branch name %q", h.Branch)
	case !commitRE.MatchString(h.Base):
		return fmt.Errorf("invalid base commit %q", h.Base)
	}
	return nil
}

// Encode returns the patch file.
func Encode(h Header, patches []string) []byte {
	return []byte(fmt.Sprintf("%s\nrepo: %s\nworktree: %s\nbranch: %s\nbase: %s\n\n%s",
		magic, h.Repo, h.Worktree, h.Branch, h.Base, strings.Join(patches, "")))
}

// Parse reads a patch file.
func Parse(data []byte) (Header, []string, error) {
	var h Header
	head, body, ok := strings.Cut(string(data), "\n\n")
	lines := strings.Split(head, "\n")
	if !ok || lines[0] != magic {
		return h, nil, errors.New("not a split-gh patch file")
	}
	fields := map[string]*string{"repo": &h.Repo, "worktree": &h.Worktree, "branch": &h.Branch, "base": &h.Base}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ": ")
		if p := fields[key]; ok && p != nil && *p == "" {
			*p = value
			continue
		}
		return h, nil, fmt.Errorf("invalid header line %q", line)
	}
	if err := h.Check(); err != nil {
		return h, nil, err
	}
	patches, err := Split(body)
	return h, patches, err
}

// Split cuts format-patch output into one text per commit.
func Split(mbox string) ([]string, error) {
	if !strings.HasPrefix(mbox, Separator) {
		return nil, errors.New("no patches in file")
	}
	chunks := strings.Split(mbox[len(Separator):], "\n"+Separator)
	patches := make([]string, len(chunks))
	for i, c := range chunks {
		patches[i] = Separator + c
		if i < len(chunks)-1 {
			patches[i] += "\n"
		}
	}
	return patches, nil
}

// formatArgs pin every option that changes format-patch output.
var formatArgs = []string{"format-patch", "--stdout", "--zero-commit", "--no-signature", "--keep-subject",
	"--no-thread", "--no-cover-letter", "--no-to", "--no-cc", "--no-add-header", "--no-signoff", "--no-base",
	"--no-notes", "--no-attach", "--encode-email-headers", "--full-index", "--no-color", "--no-ext-diff",
	"--no-textconv", "-M", "--diff-algorithm=myers", "--indent-heuristic", "--unified=3",
	"--src-prefix=a/", "--dst-prefix=b/", "--no-relative"}

// AmArgs apply patches exactly as written, whatever the user's configuration.
// A patch that `git am` would still change is caught by comparing the result.
var AmArgs = []string{"am", "--quiet", "--keep", "--keep-cr", "--no-3way",
	"--no-message-id", "--no-scissors", "--whitespace=nowarn"}

// Format returns each commit's patch text, in the order given. The text is
// empty for a commit with no changes and for a merge.
func Format(dir string, commits []string) ([]string, error) {
	// Ignore global and system configuration; the pinned options cover the rest.
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	texts := make([]string, len(commits))
	for i, c := range commits {
		out, err := Git(dir, env, append(append([]string{}, formatArgs...), "-1", c)...)
		if err != nil {
			return nil, err
		}
		texts[i] = out
	}
	return texts, nil
}

// Key is the text two patches are compared by. It drops the compressed data
// of binary changes, which depends on each qube's zlib; the full blob IDs on
// the index line still pin their content.
func Key(text string) string {
	var b strings.Builder
	inDiff, binary := false, false
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			inDiff, binary = true, false
		}
		if binary {
			continue
		}
		b.WriteString(line)
		if inDiff && line == "GIT binary patch\n" {
			binary = true
			b.WriteString("(binary data)\n")
		}
	}
	return b.String()
}

// Subject returns a patch's subject line, for messages.
func Subject(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if s, ok := strings.CutPrefix(line, "Subject: "); ok {
			return Visible(s)
		}
	}
	return "(no subject)"
}

// Visible makes control, bidirectional and other invisible characters
// visible. Otherwise a patch could hide lines from the review: less -R passes
// color escape sequences through, and bidi controls reorder text.
func Visible(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, "<0x%02X>", s[i])
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029':
			fmt.Fprintf(&b, "<U+%04X>", r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Commit is one commit of a range.
type Commit struct {
	ID      string
	Parents []string
}

// Range lists base..tip, oldest first.
func Range(dir, base, tip string) ([]Commit, error) {
	out, err := Git(dir, nil, "rev-list", "--reverse", "--parents", base+".."+tip)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if ids := strings.Fields(line); len(ids) > 0 {
			commits = append(commits, Commit{ids[0], ids[1:]})
		}
	}
	return commits, nil
}

// Linear reports whether commits form one chain on base.
func Linear(base string, commits []Commit) bool {
	parent := base
	for _, c := range commits {
		if len(c.Parents) != 1 || c.Parents[0] != parent {
			return false
		}
		parent = c.ID
	}
	return true
}

// IDs returns the commits' IDs.
func IDs(commits []Commit) []string {
	ids := make([]string, len(commits))
	for i, c := range commits {
		ids[i] = c.ID
	}
	return ids
}

// noPrograms keeps git from running programs that the files in a worktree
// can name: hooks, which a relative core.hooksPath looks for in the
// worktree, and an fsmonitor hook. In gh-auth, the worktree holds the
// agent's files. So no hook runs, the user's own included.
var noPrograms = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// Command is exec.Command for git in dir, with noPrograms. Every git that
// split-gh runs is started here.
func Command(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append(append([]string{}, noPrograms...), args...)...)
	cmd.Dir = dir
	return cmd
}

// Git runs git in dir and returns its standard output. A nil env inherits.
func Git(dir string, env []string, args ...string) (string, error) {
	cmd := Command(dir, args...)
	cmd.Env = env
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		name := ""
		for i := 0; i < len(args) && name == ""; i++ {
			if args[i] == "-c" {
				i++
			} else {
				name = args[i]
			}
		}
		return out.String(), fmt.Errorf("git %s: %s", name, msg)
	}
	return out.String(), nil
}

// Ref returns the commit a ref points to, or "" if it doesn't exist.
func Ref(dir, ref string) string {
	out, err := Git(dir, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
