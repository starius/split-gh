package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"split-gh/internal/client"
	"split-gh/internal/wire"
)

// syncBuffer collects a process's output while it runs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// freePort returns a port on 127.0.0.1 that was free a moment ago.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// serveConfig is gh-auth's config for qube ai on port, with its password
// in ~/ai.password, and the token in ~/token.
func serveConfig(port string, repos ...string) string {
	b, _ := json.Marshal(repos)
	return fmt.Sprintf(`{"qubes":{"ai":{"port":%s,"password_file":"~/ai.password","read_repos":%s}},"read_token_file":"~/token"}`, port, b)
}

// serveProcess is split-gh serve, running as the user runs it in gh-auth.
type serveProcess struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
}

// startServe runs split-gh serve and waits until it answers.
func startServe(t *testing.T, binary string, env []string) *serveProcess {
	t.Helper()
	p := &serveProcess{cmd: exec.Command(binary, "serve"), out: &syncBuffer{}, done: make(chan struct{})}
	p.cmd.Env = env
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(p.stop)
	deadline := time.After(10 * time.Second)
	for !strings.Contains(p.out.String(), "Ctrl-C stops") {
		select {
		case <-p.done:
			t.Fatalf("split-gh serve exited:\n%s", p.out.String())
		case <-deadline:
			t.Fatalf("split-gh serve didn't start:\n%s", p.out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	return p
}

// stop is Ctrl-C.
func (p *serveProcess) stop() {
	_ = p.cmd.Process.Signal(os.Interrupt)
	<-p.done
}

// This test runs the built binary as the gh shim and as split-gh serve;
// only GitHub's gh is a fake.
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
	write(filepath.Join(serverDir, "gh"), "#!/bin/sh\ncase \"$*\" in 'api meta'*) printf '{\"value\":\"ok\"}\\n';; 'run list'*) printf '[{\"databaseId\":42}]\\n';; *reviewThreads*) printf '%s\\n' '{\"data\":{\"repository\":{\"pullRequest\":{\"reviewThreads\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[{\"id\":\"T\",\"path\":\"file.go\",\"comments\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[]}}]}}}}}';; *) printf 'fake-gh %s\\n' \"$*\";; esac\n")
	port := freePort(t)
	write(filepath.Join(home, ".config/split-gh/config.json"), serveConfig(port, "o/r"))
	write(filepath.Join(home, "token"), "token")
	// Both qubes' halves share this home: gh-auth's copy of the password
	// and the agent qube's.
	write(filepath.Join(home, "ai.password"), "a-long-enough-password\n")
	write(filepath.Join(home, ".config/split-gh/password"), "a-long-enough-password\n")
	serve := startServe(t, binary, []string{"PATH=" + serverDir, "HOME=" + home})
	env := []string{"PATH=" + clientDir, "HOME=" + home, "SPLIT_GH_PORT=" + port}
	gh := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(clientDir, "gh"), args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
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
		{nil, client.About},
		{[]string{"auth", "status"}, client.About},
	} {
		if out, err := gh(tc.args...); err != nil || !strings.Contains(out, tc.want) {
			t.Errorf("%v: %s (%v)", tc.args, out, err)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"api", "-X", "POST", "repos/o/r"}, "only the user"},
		{[]string{"-R", "o/r", "pr", "comment", "123"}, "only the user"},
		{[]string{"repo", "clone", "o/r"}, `command "repo clone" is not allowed`},
		{[]string{"-R", "other/r", "pr", "view", "1"}, "repository not allowed"},
	} {
		if out, err := gh(tc.args...); err == nil || !strings.Contains(out, tc.want) || !strings.Contains(out, client.About) {
			t.Errorf("refusal %v: %q %v", tc.args, out, err)
		}
	}
	if jqPath, err := exec.LookPath("jq"); err == nil {
		if err := os.Symlink(jqPath, filepath.Join(clientDir, "jq")); err != nil {
			t.Fatal(err)
		}
		if out, err := gh("api", "meta", "--jq=.value"); err != nil || strings.TrimSpace(out) != "ok" {
			t.Errorf("local jq: %q %v", out, err)
		}
		log, err := os.ReadFile(filepath.Join(home, ".local/state/split-gh/log.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(log), "--jq") {
			t.Error("jq expression reached split-gh serve")
		}
	}
	// Requests made without the shim.
	for _, args := range [][]string{{"api", "-X", "POST", "repos/o/r"}, {"pr", "comment", "123"}, {"api", "repos/o/r", "--jq", "env.GH_TOKEN"}, {"api", "graphql", "-f", "query={viewer{login}}"}} {
		body, _ := json.Marshal(wire.Request{V: 1, Repo: "o/r", Argv: args})
		req, _ := http.NewRequest("POST", "http://127.0.0.1:"+port+"/gh", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer a-long-enough-password")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("hand-made request %v accepted: %d %q", args, resp.StatusCode, b)
		}
	}
	serve.stop()
	if !strings.Contains(serve.out.String(), "ai: gh pr view 123 --comments -> exit 0") || !strings.HasSuffix(serve.out.String(), "split-gh serve: stopped\n") {
		t.Errorf("serve's terminal:\n%s", serve.out.String())
	}
	if out, err := gh("pr", "view", "1"); err == nil || !strings.Contains(out, "nothing listens on port "+port) {
		t.Errorf("after Ctrl-C: %q %v", out, err)
	}
}
