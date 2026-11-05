package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"split-gh/internal/readpolicy"
	"split-gh/internal/wire"
)

// TestQubesPrivateRead is opt-in: it runs in the agent qube and talks to
// split-gh serve in gh-auth, and through it to GitHub. Nothing it sends can
// write: the refused POST targets a route that has no POST operation and
// has no body, and the refused push is a dry run.
func TestQubesPrivateRead(t *testing.T) {
	repoInput := os.Getenv("SPLIT_GH_E2E_REPO")
	number := os.Getenv("SPLIT_GH_E2E_PR")
	checkout := os.Getenv("SPLIT_GH_E2E_CHECKOUT")
	if repoInput == "" && number == "" && checkout == "" {
		t.Skip("set SPLIT_GH_E2E_REPO, SPLIT_GH_E2E_PR and SPLIT_GH_E2E_CHECKOUT to run the Qubes acceptance test")
	}
	repo, ok := readpolicy.CanonicalRepo(repoInput)
	pr, err := strconv.Atoi(number)
	if !ok || err != nil || pr < 1 || strconv.Itoa(pr) != number || !filepath.IsAbs(checkout) {
		t.Fatal("provide a GitHub repository, positive PR number and absolute checkout path")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	password, err := os.ReadFile(filepath.Join(home, ".config/split-gh/password"))
	if err != nil {
		t.Fatalf("the agent qube has no password: %v", err)
	}
	port := os.Getenv("SPLIT_GH_PORT")
	if port == "" {
		port = "8401"
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "split-gh")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %q: %v", out, err)
	}
	shim := filepath.Join(dir, "gh")
	if err = os.Symlink(binary, shim); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GH_REPO=")
	for _, tc := range []struct {
		name                                string
		args                                []string
		jsonArray, jsonLines, mustBePrivate bool
	}{
		{"private repository", []string{"repo", "view", "--repo=" + repo, "--json=isPrivate"}, false, false, true},
		{"pull request comments", []string{"pr", "view", number, "--comments", "--repo=" + repo}, false, false, false},
		{"REST pull request comments", []string{"api", "repos/" + repo + "/pulls/" + number + "/comments"}, true, false, false},
		{"failed run logs", []string{"run", "view", "--log-failed", "--repo=" + repo}, false, false, false},
		{"review threads", []string{"pr", "threads", number, "--repo=" + repo}, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, shim, tc.args...)
			cmd.Env = env
			cmd.Dir = checkout
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("read failed: %v; stderr=%q", err, stderr.String())
			}
			if tc.mustBePrivate {
				var metadata struct {
					IsPrivate bool `json:"isPrivate"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &metadata); err != nil || !metadata.IsPrivate {
					t.Fatalf("repository is not confirmed private: %v", err)
				}
			}
			if tc.jsonArray {
				var list []json.RawMessage
				if err := json.Unmarshal(stdout.Bytes(), &list); err != nil || list == nil {
					t.Fatalf("REST response is not a JSON array: %v", err)
				}
			}
			if tc.jsonLines {
				for _, line := range bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n")) {
					if len(line) == 0 {
						continue
					}
					var thread struct {
						Path     string          `json:"path"`
						Comments json.RawMessage `json:"comments"`
					}
					if err := json.Unmarshal(line, &thread); err != nil || thread.Path == "" || len(thread.Comments) == 0 {
						t.Fatalf("invalid review-thread response: %v", err)
					}
				}
			}
		})
	}
	for _, tc := range []struct {
		name    string
		args    []string
		message string
	}{
		{"POST", []string{"api", "-X", "POST", "repos/" + repo}, "only GET"},
		{"GraphQL", []string{"api", "graphql", "-f", "query={viewer{login}}"}, "GraphQL is not allowed"},
		{"comment", []string{"pr", "comment", number}, "not allowed"},
	} {
		t.Run("forged "+tc.name, func(t *testing.T) {
			body, err := json.Marshal(wire.Request{V: 1, Repo: repo, Argv: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:"+port+"/gh", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(password)))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("split-gh serve unreachable: %v", err)
			}
			answer, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(answer), tc.message) {
				t.Fatalf("server did not refuse %s: %d %q", tc.name, resp.StatusCode, answer)
			}
		})
	}
	// git, with the agent qube's insteadOf and password in its config.
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = checkout
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	t.Run("git ls-remote", func(t *testing.T) {
		out, err := git("ls-remote", "https://github.com/"+repo, "HEAD")
		if err != nil || len(out) < 41 || strings.Contains(out, "remote:") {
			t.Fatalf("git ls-remote: %v\n%s", err, out)
		}
	})
	t.Run("git push refused", func(t *testing.T) {
		out, err := git("push", "--dry-run", "https://github.com/"+repo, "HEAD:refs/heads/split-gh-acceptance-never")
		if err == nil || !strings.Contains(out, "can't push to GitHub") {
			t.Fatalf("push not refused: %v\n%s", err, out)
		}
	})
}

// TestQubesAllReadCommands exercises every allowed read command against real
// GitHub. The named fixtures must exist in the same private repository.
func TestQubesAllReadCommands(t *testing.T) {
	repoInput := os.Getenv("SPLIT_GH_E2E_REPO")
	checkout := os.Getenv("SPLIT_GH_E2E_CHECKOUT")
	pr := os.Getenv("SPLIT_GH_E2E_PR")
	issue := os.Getenv("SPLIT_GH_E2E_ISSUE")
	run := os.Getenv("SPLIT_GH_E2E_RUN")
	workflow := os.Getenv("SPLIT_GH_E2E_WORKFLOW")
	release := os.Getenv("SPLIT_GH_E2E_RELEASE")
	// These extra fixture values opt in independently of the core acceptance test.
	if issue == "" && run == "" && workflow == "" && release == "" {
		t.Skip("set the issue, run, workflow and release fixture variables to run the full real-GitHub command matrix")
	}
	repo, ok := readpolicy.CanonicalRepo(repoInput)
	positive := func(s string) bool { n, err := strconv.Atoi(s); return err == nil && n > 0 && strconv.Itoa(n) == s }
	if !ok || !filepath.IsAbs(checkout) || !positive(pr) || !positive(issue) || !positive(run) || workflow == "" || release == "" {
		t.Fatal("provide the repository, checkout, positive PR, issue and run IDs, workflow and release fixture variables")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "split-gh")
	shim := filepath.Join(dir, "gh")
	if out, err := exec.Command("go", "build", "-trimpath", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %q: %v", out, err)
	}
	if err := os.Symlink(binary, shim); err != nil {
		t.Fatal(err)
	}
	repoFlag := "--repo=" + repo
	cases := []struct {
		name string
		args []string
	}{
		{"pr view", []string{"pr", "view", pr, repoFlag}},
		{"pr list", []string{"pr", "list", repoFlag}},
		{"pr diff", []string{"pr", "diff", pr, "--name-only", repoFlag}},
		{"pr checks", []string{"pr", "checks", pr, repoFlag}},
		{"pr threads", []string{"pr", "threads", pr, repoFlag}},
		{"issue view", []string{"issue", "view", issue, repoFlag}},
		{"issue list", []string{"issue", "list", repoFlag}},
		{"repo view", []string{"repo", "view", repoFlag, "--json=isPrivate"}},
		{"label list", []string{"label", "list", repoFlag}},
		{"release list", []string{"release", "list", repoFlag}},
		{"release view", []string{"release", "view", release, repoFlag}},
		{"run list", []string{"run", "list", repoFlag}},
		{"run view", []string{"run", "view", run, repoFlag}},
		{"workflow list", []string{"workflow", "list", repoFlag}},
		{"workflow view", []string{"workflow", "view", workflow, repoFlag}},
		{"search issues", []string{"search", "issues", "test", repoFlag}},
		{"search prs", []string{"search", "prs", "test", repoFlag}},
		{"search commits", []string{"search", "commits", "test", repoFlag}},
		{"search code", []string{"search", "code", "test", repoFlag}},
		{"api", []string{"api", "repos/" + repo + "/pulls/" + pr + "/comments"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, shim, tc.args...)
			cmd.Dir = checkout
			cmd.Env = append(os.Environ(), "GH_REPO=")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("read failed: %v; stderr=%q", err, stderr.String())
			}
			if tc.name == "repo view" {
				var metadata struct {
					IsPrivate bool `json:"isPrivate"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &metadata); err != nil || !metadata.IsPrivate {
					t.Fatalf("repository is not confirmed private: %v", err)
				}
			}
		})
	}
}
