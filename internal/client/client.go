package client

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"split-gh/internal/readpolicy"
	"split-gh/internal/wire"
	"strings"
)

var remoteRE = regexp.MustCompile(`(?i)^(?:https://github\.com/|ssh://git@github\.com/|git@github\.com:)([^/]+/[^/]+?)(?:\.git)?/?$`)

func git(args ...string) string {
	b, e := exec.Command("git", args...).Output()
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func Main(args []string) int {
	if locallyRefusedWrite(args) {
		fmt.Fprintln(os.Stderr, "writes to GitHub are done by the user")
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

	if repo == "" {
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
	if repo == "" && len(args) > 1 && args[0] == "api" {
		for _, a := range args[1:] {
			p := strings.Split(a, "/")
			if len(p) >= 3 && p[0] == "repos" {
				repo = p[1] + "/" + p[2]
				break
			}
		}
	}
	target := os.Getenv("SPLIT_GH_QUBE")
	if target == "" {
		target = "gh-auth"
	}
	cmd := exec.Command("qrexec-client-vm", target, "splitgh.Read")
	stdin, e := cmd.StdinPipe()
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	cmd.Stderr = os.Stderr
	if e = cmd.Start(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	json.NewEncoder(stdin).Encode(wire.Request{V: 1, Argv: args, Repo: repo, Branch: git("branch", "--show-current")})
	stdin.Close()
	code, e := wire.Receive(stdout, output, os.Stderr)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		code = 1
		stdout.Close()
		_ = cmd.Process.Kill()
	}
	if e = cmd.Wait(); e != nil && code == 0 {
		code = 1
	}
	if jq != nil {
		jqInput.Close()
		if err := jq.Wait(); err != nil && code == 0 {
			code = 1
		}
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
