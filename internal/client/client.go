package client

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"split-gh/internal/readpolicy"
	"split-gh/internal/wire"
	"strings"
)

var remoteRE = regexp.MustCompile(`^(?:https://github\.com/|ssh://git@github\.com/|git@github\.com:)([^/]+/[^/]+?)(?:\.git)?/?$`)

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
	if repo == "" {
		for _, r := range []string{"gh-resolved", "upstream", "origin"} {
			url := git("remote", "get-url", r)
			if m := remoteRE.FindStringSubmatch(url); m != nil {
				repo = m[1]
				break
			}
		}
	}
	for i, a := range args {
		if strings.HasPrefix(a, "-R") && len(a) > 2 {
			repo = strings.TrimPrefix(strings.TrimPrefix(a, "-R"), "=")
		}
		if strings.HasPrefix(a, "--repo=") {
			repo = strings.TrimPrefix(a, "--repo=")
		}
		if (a == "--repo" || a == "-R") && i+1 < len(args) {
			repo = args[i+1]
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
