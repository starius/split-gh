package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"split-gh/internal/readpolicy"
	"split-gh/internal/wire"
)

// About is what the shim tells agents about itself: for gh, gh help and
// gh auth status, and after every refusal.
const About = `This gh is split-gh. The real gh runs in the user's gh-auth qube with a
read-only token, and most read commands work as usual: gh pr view, gh pr diff,
gh pr checks, gh pr list, gh issue view, gh run view --log-failed,
gh search ... --repo OWNER/REPO, and gh api repos/OWNER/REPO/... (GET only).
For review threads with their resolved state, use gh pr threads NUMBER.
gh project view NUMBER --owner OWNER reads projects allowed in read_projects.
git clone and git fetch from GitHub work too: through gh-auth for the
repositories it may read, and straight from GitHub for other public ones.
Instead of gh repo clone and gh pr checkout, use
git clone https://github.com/OWNER/REPO and git fetch origin pull/NUMBER/head.

You can't write to GitHub: no push, PR, comment, review, edit or merge. The
user does that. To hand over commits, commit in a git worktree, run
split-gh send there and show the user the commands it prints. For a PR
description or a comment, write the text in your reply for the user to post.
`

// remoteRE matches GitHub remote URLs, also as insteadOf makes them for
// git-remote-split-gh: git remote get-url applies insteadOf.
var remoteRE = regexp.MustCompile(`(?i)^(?:https://github\.com/|ssh://git@github\.com/|git@github\.com:|split-gh::https://github\.com/)([^/]+/[^/]+?)(?:\.git)?/?$`)

func git(args ...string) string {
	b, e := exec.Command("git", args...).Output()
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Main is the gh shim.
func Main(args []string) int {
	if aboutRequest(args) {
		fmt.Print(About)
		return 0
	}
	if locallyRefusedWrite(args) {
		fmt.Fprint(os.Stderr, "gh: that would write to GitHub, which only the user does.\n\n"+About)
		return 1
	}
	cleanArgs, expression, hasJQ, e := readpolicy.ExtractClientJQ(args)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	args = cleanArgs
	var jq *exec.Cmd
	var jqInput io.WriteCloser
	output := io.Writer(os.Stdout)
	if hasJQ {
		jq = exec.Command("jq", "-r", "--", expression)
		jq.Stdout = os.Stdout
		jq.Stderr = os.Stderr
		jqInput, e = jq.StdinPipe()
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		if e = jq.Start(); e != nil {
			fmt.Fprintln(os.Stderr, "--jq requires jq in the agent qube:", e)
			return 1
		}
		defer func() { _ = jqInput.Close(); _ = jq.Process.Kill(); _ = jq.Wait() }()
		output = jqInput
	}
	repo := os.Getenv("GH_REPO")
	if canonical, ok := readpolicy.CanonicalRepo(repo); ok {
		repo = canonical
	}
	if explicit, ok := readpolicy.ClientRepoFlag(args); ok {
		repo = explicit
	}

	projectView := len(args) >= 2 && args[0] == "project" && args[1] == "view"
	if repo == "" && !projectView {
		var err error
		repo, err = inferRepo()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if canonical, ok := readpolicy.CanonicalRepo(repo); ok {
		repo = canonical
	}
	if repo == "" {
		repo = repoFromAPIArgs(args)
	}
	code := ask(wire.Request{V: 1, Argv: args, Repo: repo, Branch: git("branch", "--show-current")}, output)
	if jq != nil {
		jqInput.Close()
		if err := jq.Wait(); err != nil && code == 0 {
			code = 1
		}
	}
	return code
}

// aboutRequest reports whether the agent asks what this gh is.
func aboutRequest(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "help", "--help", "-h", "version", "--version":
		return true
	}
	return len(args) == 2 && args[0] == "auth" && args[1] == "status"
}

// port is where qvm-connect-tcp forwards to split-gh serve in gh-auth.
func port() string {
	if p := os.Getenv("SPLIT_GH_PORT"); p != "" {
		return p
	}
	return "8401"
}

// ask sends one request to split-gh serve and copies the answer to out and
// stderr. It returns gh's exit status.
func ask(q wire.Request, out io.Writer) int {
	home, _ := os.UserHomeDir()
	password, err := os.ReadFile(filepath.Join(home, ".config/split-gh/password"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh: split-gh isn't set up in this qube: %v. Ask the user.\n", err)
		return 1
	}
	body, err := json.Marshal(q)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh:", err)
		return 1
	}
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+port()+"/gh", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh:", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(password)))
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if errors.Is(err, syscall.ECONNREFUSED) {
		fmt.Fprintf(os.Stderr, "gh: can't reach split-gh: nothing listens on port %s in this qube.\n"+
			"qvm-connect-tcp must forward it to gh-auth; ask the user to start it.\n", port())
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh: split-gh didn't answer (%v).\n"+
			"Probably split-gh serve isn't running in gh-auth; ask the user to start it.\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		fmt.Fprintf(os.Stderr, "gh: %s\n", strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusForbidden {
			fmt.Fprint(os.Stderr, "\n"+About)
		}
		return 1
	}
	code, err := wire.Receive(resp.Body, out, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh: split-gh's answer broke off:", err)
		return 1
	}
	return code
}

// This is a convenience only; the trusted server independently validates
// every request, including requests constructed without this shim.
func locallyRefusedWrite(args []string) bool {
	normalized, err := readpolicy.NormalizeGlobal(args)
	if err != nil || len(normalized) == 0 {
		return false
	}
	args = normalized
	if len(args) >= 2 {
		writes := map[string]map[string]bool{"pr": {"create": true, "comment": true, "review": true, "edit": true, "merge": true, "close": true, "reopen": true, "ready": true}, "issue": {"create": true, "comment": true, "edit": true, "close": true, "reopen": true, "delete": true}, "release": {"create": true, "edit": true, "delete": true}, "label": {"create": true, "edit": true, "delete": true}}
		if writes[args[0]][args[1]] {
			return true
		}
	}
	if args[0] != "api" {
		return false
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--input" || strings.HasPrefix(a, "--input=") {
			return true
		}
		method := ""
		switch {
		case a == "-X" || a == "--method":
			if i+1 < len(args) {
				i++
				method = args[i]
			}
		case strings.HasPrefix(a, "-X") && len(a) > 2:
			method = strings.TrimPrefix(a[2:], "=")
		case strings.HasPrefix(a, "--method="):
			method = strings.TrimPrefix(a, "--method=")
		case a == "-f" || a == "-F" || a == "-H" || a == "--field" || a == "--raw-field" || a == "--header" || a == "--jq" || a == "--template":
			i++
		}
		if method != "" && !strings.EqualFold(method, "GET") {
			return true
		}
	}
	return false
}

// inferRepo respects gh's persisted base resolution before the usual remotes.
// Every result is advisory: the server checks the repository again.
func inferRepo() (string, error) {
	type resolved struct{ name, value string }
	var entries []resolved
	indexes := map[string]int{}
	config := git("config", "--get-regexp", `^remote\..*\.gh-resolved$`)
	for _, line := range strings.Split(config, "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(key, "remote.") || !strings.HasSuffix(key, ".gh-resolved") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".gh-resolved")
		if name != "" && value != "" {
			value = strings.TrimSpace(value)
			if index, exists := indexes[name]; exists {
				entries[index].value = value
			} else {
				indexes[name] = len(entries)
				entries = append(entries, resolved{name, value})
			}
		}
	}
	rank := func(name string) int {
		switch strings.ToLower(name) {
		case "upstream":
			return 3
		case "github":
			return 2
		case "origin":
			return 1
		}
		return 0
	}
	sort.SliceStable(entries, func(i, j int) bool { return rank(entries[i].name) > rank(entries[j].name) })
	repoFor := func(name string) string {
		url := git("remote", "get-url", "--", name)
		if m := remoteRE.FindStringSubmatch(url); m != nil {
			return m[1]
		}
		return ""
	}
	for _, entry := range entries {
		remoteRepo := repoFor(entry.name)
		if remoteRepo == "" {
			continue
		}
		if entry.value == "base" {
			return remoteRepo, nil
		}
		if canonical, ok := readpolicy.CanonicalRepo(entry.value); ok {
			return canonical, nil
		}
		if m := remoteRE.FindStringSubmatch(entry.value); m != nil {
			return m[1], nil
		}
		return "", fmt.Errorf("invalid gh-resolved repository for remote %q", entry.name)
	}
	for _, name := range []string{"upstream", "origin"} {
		if repo := repoFor(name); repo != "" {
			return repo, nil
		}
	}
	return "", nil
}

func repoFromAPIArgs(args []string) string {
	if len(args) < 2 || args[0] != "api" {
		return ""
	}
	for _, arg := range args[1:] {
		endpoint, _, _ := strings.Cut(arg, "?")
		parts := strings.Split(endpoint, "/")
		if len(parts) >= 3 && parts[0] == "repos" {
			if repo, ok := readpolicy.CanonicalRepo(parts[1] + "/" + parts[2]); ok {
				return repo
			}
		}
	}
	return ""
}
