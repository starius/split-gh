package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestQubesPrivateRead is opt-in: it talks to the installed trusted service
// and GitHub. The refused POST targets a route that has no POST operation and
// has no body, so it cannot create a comment even if the service is broken.
func TestQubesPrivateRead(t *testing.T) {
	repoInput := os.Getenv("SPLIT_GH_E2E_REPO")
	number := os.Getenv("SPLIT_GH_E2E_PR")
	qube := os.Getenv("SPLIT_GH_E2E_QUBE")
	checkout := os.Getenv("SPLIT_GH_E2E_CHECKOUT")
	if repoInput == "" && number == "" && qube == "" && checkout == "" {
		t.Skip("set SPLIT_GH_E2E_REPO, SPLIT_GH_E2E_PR, SPLIT_GH_E2E_QUBE and SPLIT_GH_E2E_CHECKOUT to run the Qubes acceptance test")
	}
	repo, ok := readpolicy.CanonicalRepo(repoInput)
	pr, err := strconv.Atoi(number)
	if !ok || err != nil || pr < 1 || strconv.Itoa(pr) != number || qube == "" || !filepath.IsAbs(checkout) {
		t.Fatal("provide a GitHub repository, positive PR number, target qube and absolute checkout path")
	}
	if _, err = exec.LookPath("qrexec-client-vm"); err != nil {
		t.Fatalf("qrexec-client-vm unavailable: %v", err)
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
	env := append(os.Environ(), "SPLIT_GH_QUBE="+qube, "GH_REPO=")
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
			var header bytes.Buffer
			if err := json.NewEncoder(&header).Encode(wire.Request{V: 1, Repo: repo, Argv: tc.args}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "qrexec-client-vm", qube, "splitgh.Read")
			cmd.Stdin = &header
			var response, transportErr bytes.Buffer
			cmd.Stdout = &response
			cmd.Stderr = &transportErr
			if err := cmd.Run(); err != nil {
				t.Fatalf("qrexec failed: %v; stderr=%q", err, transportErr.String())
			}
			var stdout, stderr bytes.Buffer
			status, err := wire.Receive(&response, &stdout, &stderr)
			if err != nil || status == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.message) {
				t.Fatalf("server did not refuse %s: status=%d stderr=%q framing=%v", tc.name, status, stderr.String(), err)
			}
		})
	}
}
