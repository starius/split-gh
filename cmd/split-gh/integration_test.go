package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"split-gh/internal/wire"
)

// This test exercises the actual executable, shim dispatch, framed qrexec
// transport and trusted service; only GitHub's gh process is substituted.
func TestReadPathEndToEnd(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "split-gh")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s: %v", output, err)
	}
	clientDir := filepath.Join(root, "client")
	serverDir := filepath.Join(root, "server")
	home := filepath.Join(root, "home")
	for _, dir := range []string{clientDir, serverDir, filepath.Join(home, ".config/split-gh")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(binary, filepath.Join(clientDir, "gh")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(clientDir, "git"), "#!/bin/sh\ncase \"$*\" in 'remote get-url -- origin') echo https://github.com/o/r.git;; 'branch --show-current') echo main;; esac\n")
	write(filepath.Join(clientDir, "qrexec-client-vm"), "#!/bin/sh\nexport QREXEC_REMOTE_DOMAIN=ai\nexport PATH=\"$SPLIT_TEST_SERVER_PATH\"\nexec \"$SPLIT_TEST_BINARY\" serve read\n")
	write(filepath.Join(serverDir, "gh"), "#!/bin/sh\ncase \"$*\" in 'api meta'*) printf '{\"value\":\"ok\"}\\n';; 'run list'*) printf '[{\"databaseId\":42}]\\n';; *reviewThreads*) printf '%s\\n' '{\"data\":{\"repository\":{\"pullRequest\":{\"reviewThreads\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[{\"id\":\"T\",\"path\":\"file.go\",\"comments\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[]}}]}}}}}';; *) printf 'fake-gh %s\\n' \"$*\";; esac\n")
	write(filepath.Join(home, ".config/split-gh/config.json"), `{"qubes":{"ai":{"read_repos":["o/r"]}},"read_token_file":"~/token"}`)
	write(filepath.Join(home, "token"), "token")
	env := append(os.Environ(), "PATH="+clientDir, "HOME="+home, "SPLIT_TEST_SERVER_PATH="+serverDir, "SPLIT_TEST_BINARY="+binary)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "view", "123", "--comments"}, "--repo=o/r"},
		{[]string{"-Ro/r", "pr", "view", "123"}, "--repo=o/r"},
		{[]string{"api", "repos/{owner}/{repo}/pulls/123/comments"}, "repos/o/r/pulls/123/comments"},
		{[]string{"-R", "o/r", "pr", "threads", "123"}, "file.go"},
		{[]string{"api", "repos/o/r/pulls/123/comments"}, "--method=GET"},
		{[]string{"run", "view", "--log-failed"}, "run view 42"},
		{[]string{"pr", "threads", "123"}, "file.go"},
	} {
		cmd := exec.Command(filepath.Join(clientDir, "gh"), tc.args...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(output), tc.want) {
			t.Errorf("%v: %s (%v)", tc.args, output, err)
		}
	}
	for _, args := range [][]string{{"api", "-X", "POST", "repos/o/r"}, {"-R", "o/r", "pr", "comment", "123"}} {
		cmd := exec.Command(filepath.Join(clientDir, "gh"), args...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "writes to GitHub are done by the user") {
			t.Errorf("local refusal %v: %q %v", args, output, err)
		}
	}
	if jqPath, err := exec.LookPath("jq"); err == nil {
		if err := os.Symlink(jqPath, filepath.Join(clientDir, "jq")); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(filepath.Join(clientDir, "gh"), "api", "meta", "--jq=.value")
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != "ok" {
			t.Errorf("local jq: %q %v", output, err)
		}
		log, err := os.ReadFile(filepath.Join(home, ".local/state/split-gh/log.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(log), "--jq") {
			t.Error("jq expression reached trusted service")
		}
	}
	for _, args := range [][]string{{"api", "-X", "POST", "repos/o/r"}, {"pr", "comment", "123"}, {"api", "repos/o/r", "--jq", "env.GH_TOKEN"}, {"api", "graphql", "-f", "query={viewer{login}}"}} {
		var request bytes.Buffer
		json.NewEncoder(&request).Encode(wire.Request{V: 1, Repo: "o/r", Argv: args})
		cmd := exec.Command(binary, "serve", "read")
		cmd.Env = append(env, "QREXEC_REMOTE_DOMAIN=ai")
		cmd.Stdin = &request
		response, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code, err := wire.Receive(bytes.NewReader(response), &stdout, &stderr)
		if err != nil || code == 0 || stdout.Len() != 0 {
			t.Errorf("hand-modified request %v accepted: %d %q %q %v", args, code, stdout.String(), stderr.String(), err)
		}
	}
}
