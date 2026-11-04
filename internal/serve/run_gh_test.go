package serve

import (
	"archive/zip"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"split-gh/internal/readpolicy"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRealGHRunViewFailedLogs(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	var mu sync.Mutex
	var paths []string
	run := `{"id":42,"name":"CI","display_title":"CI","run_number":1,"run_attempt":1,"event":"push","status":"completed","conclusion":"failure","head_branch":"main","head_sha":"abc","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:01:00Z","html_url":"https://github.com/o/r/actions/runs/42","workflow_id":5}`
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runWithURLs := strings.TrimSuffix(run, "}") + fmt.Sprintf(`,"jobs_url":%q,"logs_url":%q}`, server.URL+"/api/v3/repos/o/r/actions/runs/42/jobs", server.URL+"/api/v3/repos/o/r/actions/runs/42/logs")
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/meta"):
			io.WriteString(w, `{"installed_version":"3.14.0"}`)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows/5"):
			io.WriteString(w, `{"id":5,"name":"CI","path":".github/workflows/ci.yml","state":"active"}`)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			io.WriteString(w, `{"total_count":1,"workflows":[{"id":5,"name":"CI","path":".github/workflows/ci.yml","state":"active"}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			if r.URL.Query().Get("branch") != "main" {
				t.Errorf("latest run lookup did not filter checkout branch: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"total_count":1,"workflow_runs":[`+runWithURLs+`]}`)
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
		case strings.HasSuffix(r.URL.Path, "/actions/jobs/7/logs"):
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "2026-01-01T00:00:00Z failure detail\n")
		case strings.HasSuffix(r.URL.Path, "/actions/runs/42"):
			io.WriteString(w, runWithURLs)
		default:
			http.Error(w, "unexpected "+r.URL.Path, 404)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	cert := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + dir, "GH_CONFIG_DIR=" + dir, "GH_HOST=" + strings.TrimPrefix(server.URL, "https://"), "GH_ENTERPRISE_TOKEN=test-token", "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "NO_COLOR=1", "SSL_CERT_FILE=" + cert, "NO_PROXY=*", "PATH=" + os.Getenv("PATH")}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	runGH := func(args []string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, gh, args...)
		cmd.Env = env
		cmd.Dir = dir
		return cmd.CombinedOutput()
	}
	data, err := runGH(latestRunArgs("o/r", "main"))
	if err != nil {
		t.Fatalf("list: %q %v", data, err)
	}
	id, err := latestRunID(data)
	if err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	args, err := readpolicy.Parse([]string{"run", "view", fmt.Sprint(id), "--log-failed"}, "o/r", "main", []string{"o/*"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := runGH(args)
	if err != nil || !strings.Contains(string(output), "failure detail") {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("view: %q %v paths=%v", output, err, paths)
	}
}
