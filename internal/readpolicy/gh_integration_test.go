package readpolicy

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the actual gh argument parser and HTTP behavior without making any
// external requests or requiring a token. The test is optional where gh is not
// installed.
func TestRealGHUsesGET(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	type request struct {
		method, path, query string
		body                []byte
	}
	requests := make(chan request, 2)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- request{r.Method, r.URL.Path, r.URL.RawQuery, body}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"repository":{"name":"r"}}}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	cases := [][]string{
		{"api", "repos/o/r/pulls/1/comments", "-f", "per_page=10"},
		{"api", "repos/o/r/pulls/1/comments?per_page=20", "-f", "page=2"},
	}
	for _, args := range cases {
		canonical, err := Parse(args, "o/r", "main", []string{"o/*"})
		if err != nil {
			t.Fatalf("policy %v: %v", args, err)
		}
		cmd := exec.Command(gh, canonical...)
		cmd.Env = []string{"HOME=" + dir, "GH_CONFIG_DIR=" + dir, "GH_HOST=" + host, "GH_ENTERPRISE_TOKEN=test-token", "GH_PROMPT_DISABLED=1", "SSL_CERT_FILE=" + certPath, "NO_PROXY=*", "PATH=" + os.Getenv("PATH")}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gh %v: %s (%v)", args, out, err)
		}
		got := <-requests
		if got.method != "GET" || len(got.body) != 0 {
			t.Fatalf("unexpected request: %+v", got)
		}
		if strings.Contains(strings.Join(args, " "), "per_page=20") {
			params, err := url.ParseQuery(got.query)
			if err != nil || params.Get("per_page") != "20" || params.Get("page") != "2" {
				t.Fatalf("query not preserved: %q %v", got.query, err)
			}
		}

	}
}

// gh's jq implementation can access environment variables. Confirm the
// trusted policy rejects jq without relying on jq expression filtering.
func TestGHFormattingTokenBoundary(t *testing.T) {
	for _, args := range [][]string{{"api", "meta", "--jq", "env.GH_TOKEN"}, {"api", "meta", "--jq", "$ENV.GH_TOKEN"}} {
		if _, err := Parse(args, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("unsafe formatting accepted: %v", args)
		}
	}
}

func TestRealGHTemplateDoesNotExposeEnvironment(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"value":"ok"}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	cert := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{`{{env "GH_TOKEN"}}`, `{{expandenv "$GH_TOKEN"}}`} {
		args, err := Parse([]string{"api", "meta", "--template", format}, "o/r", "main", []string{"o/*"})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(gh, args...)
		cmd.Env = []string{"HOME=" + dir, "GH_CONFIG_DIR=" + dir, "GH_HOST=" + strings.TrimPrefix(server.URL, "https://"), "GH_ENTERPRISE_TOKEN=test-token", "GH_TOKEN=secret-sentinel", "GH_PROMPT_DISABLED=1", "SSL_CERT_FILE=" + cert, "NO_PROXY=*", "PATH=" + os.Getenv("PATH")}
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), "secret-sentinel") {
			t.Errorf("template exposed token: %q", out)
		}
	}
}

func TestRealGHSearchCombinesQualifierWithQuery(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	queries := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/meta") {
			io.WriteString(w, `{"installed_version":"3.14.0"}`)
			return
		}
		queries <- r.URL.Query().Get("q")
		io.WriteString(w, `{"total_count":0,"incomplete_results":false,"items":[]}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	cert := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gh, "search", "issues", "foo OR bar", "--repo=o/r")
	cmd.Env = []string{"HOME=" + dir, "GH_CONFIG_DIR=" + dir, "GH_HOST=" + strings.TrimPrefix(server.URL, "https://"), "GH_ENTERPRISE_TOKEN=test-token", "GH_PROMPT_DISABLED=1", "SSL_CERT_FILE=" + cert, "NO_PROXY=*", "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gh search: %s (%v)", out, err)
	}
	query := <-queries
	if !strings.Contains(query, "foo OR bar") || !strings.Contains(query, "repo:o/r") {
		t.Fatalf("unexpected GitHub query: %q", query)
	}
	if _, err := Parse([]string{"search", "issues", "foo OR bar", "--repo=o/r"}, "o/r", "main", []string{"o/*"}); err == nil {
		t.Fatal("unsafe ungrouped disjunction accepted")
	}
}

func TestRealGHPRViewComments(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	var requests atomic.Int32
	var bodiesMu sync.Mutex
	var bodies []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		data, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, r.URL.Path+" "+string(data))
		bodiesMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/meta") {
			io.WriteString(w, `{"installed_version":"3.14.0"}`)
			return
		}
		if strings.Contains(string(data), "PullRequestProjectItems") {
			io.WriteString(w, `{"data":{"repository":{"pullRequest":{"projectItems":{"totalCount":0,"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`)
			return
		}
		io.WriteString(w, `{"data":{"repository":{"pullRequest":{"id":"PR1","number":123,"title":"Example pull request","body":"Description","state":"OPEN","url":"https://github.com/o/r/pull/123","author":{"login":"alice"},"baseRefName":"main","headRefName":"feature","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","comments":{"nodes":[{"id":"IC1","author":{"login":"alice"},"authorAssociation":"MEMBER","body":"Review comment text","createdAt":"2026-01-01T00:00:00Z","includesCreatedEdit":false,"isMinimized":false,"minimizedReason":null,"reactionGroups":[],"url":"https://github.com/o/r/pull/123#issuecomment-1","viewerDidAuthor":false}],"pageInfo":{"hasNextPage":false,"endCursor":null},"totalCount":1},"reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"latestReviews":{"nodes":[]},"labels":{"nodes":[]},"assignees":{"nodes":[]}}}}}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	cert := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	args, err := Parse([]string{"pr", "view", "123", "--comments"}, "o/r", "main", []string{"o/*"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, args...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "GH_CONFIG_DIR=" + dir, "GH_HOST=" + strings.TrimPrefix(server.URL, "https://"), "GH_ENTERPRISE_TOKEN=test-token", "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "NO_COLOR=1", "SSL_CERT_FILE=" + cert, "NO_PROXY=*", "PATH=" + os.Getenv("PATH")}
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Review comment text") || requests.Load() == 0 {
		bodiesMu.Lock()
		defer bodiesMu.Unlock()
		t.Fatalf("gh pr view: %q (%v), requests=%d requests=%q", output, err, requests.Load(), bodies)
	}
}
