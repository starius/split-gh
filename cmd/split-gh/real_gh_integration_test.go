package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Uses real gh, the built shim and service, and a qrexec stand-in. GitHub is
// represented by an HTTPS fixture; no credentials or external network are used.
func TestReadPathWithRealGH(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "split-gh")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	run := `{"id":42,"name":"CI","display_title":"CI","run_number":1,"run_attempt":1,"event":"push","status":"completed","conclusion":"failure","head_branch":"main","head_sha":"abc","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:01:00Z","html_url":"https://github.com/o/r/actions/runs/42","workflow_id":5}`
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/meta"):
			io.WriteString(w, `{"installed_version":"3.14.0"}`)
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			var p struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Errorf("GraphQL request: %v", err)
				http.Error(w, "bad body", 400)
				return
			}
			switch {
			case strings.Contains(p.Query, "PullRequestProjectItems"):
				io.WriteString(w, `{"data":{"repository":{"pullRequest":{"projectItems":{"totalCount":0,"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`)
			case strings.Contains(p.Query, "PullRequestByNumber"):
				io.WriteString(w, `{"data":{"repository":{"pullRequest":{"id":"PR1","number":123,"title":"Example","body":"Description","state":"OPEN","url":"https://github.com/o/r/pull/123","author":{"login":"alice"},"baseRefName":"main","headRefName":"feature","isDraft":false,"comments":{"nodes":[{"id":"IC1","author":{"login":"alice"},"authorAssociation":"MEMBER","body":"PR comment","createdAt":"2026-01-01T00:00:00Z","includesCreatedEdit":false,"isMinimized":false,"minimizedReason":null,"reactionGroups":[],"url":"https://github.com/o/r/pull/123#issuecomment-1","viewerDidAuthor":false}],"pageInfo":{"hasNextPage":false,"endCursor":null},"totalCount":1},"reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"labels":{"nodes":[]},"assignees":{"nodes":[]}}}}}`)
			case strings.Contains(p.Query, "reviewThreads(first:100"):
				if r.Method != "POST" || p.Variables["owner"] != "o" || p.Variables["name"] != "r" || p.Variables["number"] != float64(123) {
					t.Errorf("thread variables: method=%s vars=%v", r.Method, p.Variables)
				}
				io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"T","isResolved":false,"isOutdated":false,"path":"file.go","line":4,"comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"body":"thread comment"}]}}]}}}}}`)
			default:
				http.Error(w, "unexpected GraphQL query", 400)
			}
		case strings.HasSuffix(r.URL.Path, "/pulls/123/comments"):
			if r.Method != "GET" {
				t.Error("REST request was not GET")
			}
			io.WriteString(w, `[{"id":1,"body":"REST comment"}]`)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/5"):
			io.WriteString(w, `{"id":5,"name":"CI","path":".github/workflows/ci.yml","state":"active"}`)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			io.WriteString(w, `{"total_count":1,"workflows":[{"id":5,"name":"CI","path":".github/workflows/ci.yml","state":"active"}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			if r.URL.Query().Get("branch") != "main" {
				t.Errorf("unexpected branch: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"total_count":1,"workflow_runs":[`+run+`]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs/42/jobs"):
			io.WriteString(w, `{"total_count":1,"jobs":[{"id":7,"name":"test","status":"completed","conclusion":"failure","started_at":"2026-01-01T00:00:00Z","completed_at":"2026-01-01T00:01:00Z","steps":[{"name":"test","number":1,"status":"completed","conclusion":"failure"}]}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs/42/logs"):
			w.Header().Set("Content-Type", "application/zip")
			z := zip.NewWriter(w)
			entry, err := z.Create("test/1_test.txt")
			if err != nil {
				t.Error(err)
				return
			}
			io.WriteString(entry, "2026-01-01T00:00:00Z failure detail\n")
			if err = z.Close(); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/actions/runs/42"):
			withURLs := strings.TrimSuffix(run, "}") + fmt.Sprintf(`,"jobs_url":%q,"logs_url":%q}`, server.URL+"/api/v3/repos/o/r/actions/runs/42/jobs", server.URL+"/api/v3/repos/o/r/actions/runs/42/logs")
			io.WriteString(w, withURLs)
		default:
			http.Error(w, "unexpected "+r.URL.Path, 404)
		}
	}))
	defer server.Close()
	clientDir, serverDir, home := filepath.Join(root, "client"), filepath.Join(root, "server"), filepath.Join(root, "home")
	for _, dir := range []string{clientDir, serverDir, filepath.Join(home, ".config/split-gh")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(binary, filepath.Join(clientDir, "gh")); err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	write(filepath.Join(clientDir, "git"), "#!/bin/sh\ncase \"$*\" in 'remote get-url -- origin') echo https://github.com/o/r.git;; 'branch --show-current') echo main;; esac\n")
	write(filepath.Join(clientDir, "qrexec-client-vm"), "#!/bin/sh\nexport QREXEC_REMOTE_DOMAIN=ai\nexport PATH="+quote(serverDir)+"\nexec "+quote(binary)+" serve read\n")
	write(filepath.Join(serverDir, "gh"), "#!/bin/sh\nexport GH_HOST="+quote(strings.TrimPrefix(server.URL, "https://"))+"\nexport GH_ENTERPRISE_TOKEN=\"$GH_TOKEN\"\nexport SSL_CERT_FILE="+quote(cert)+"\nexport NO_PROXY='*'\nexec "+quote(gh)+" \"$@\"\n")
	write(filepath.Join(home, ".config/split-gh/config.json"), `{"qubes":{"ai":{"read_repos":["o/r"]}},"read_token_file":"~/token"}`)
	write(filepath.Join(home, "token"), "test-token")
	env := append(os.Environ(), "PATH="+clientDir, "HOME="+home, "GH_REPO=")
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"pr", "view", "123", "--comments"}, "PR comment"}, {[]string{"api", "repos/o/r/pulls/123/comments"}, "REST comment"}, {[]string{"run", "view", "--log-failed"}, "failure detail"}, {[]string{"pr", "threads", "123"}, "thread comment"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, filepath.Join(clientDir, "gh"), tc.args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil || !strings.Contains(string(out), tc.want) {
			t.Errorf("gh %v: %q %v", tc.args, out, err)
		}
	}
}
