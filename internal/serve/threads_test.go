package serve

import (
	"bytes"
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
	"sync/atomic"
	"testing"
	"time"
)

func TestThreadsPagination(t *testing.T) {
	calls := 0
	fetch := func(query string, fields ...string) ([]byte, error) {
		calls++
		switch calls {
		case 1:
			if query != threadsQuery {
				return nil, fmt.Errorf("unexpected query")
			}
			return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"t1"},"nodes":[{"id":"thread1","path":"a.go","comments":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[{"body":"first"}]}}]}}}}}`), nil
		case 2:
			if query != threadsQuery || !strings.Contains(strings.Join(fields, " "), "id=thread1") {
				return nil, fmt.Errorf("bad comment request %v", fields)
			}
			return []byte(`{"data":{"node":{"comments":{"pageInfo":{"hasNextPage":false},"nodes":[{"body":"second"}]}}}}`), nil
		case 3:
			if query != threadsQuery || !strings.Contains(strings.Join(fields, " "), "cursor=t1") {
				return nil, fmt.Errorf("bad thread cursor %v", fields)
			}
			return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"thread2","path":"b.go","comments":{"pageInfo":{"hasNextPage":false},"nodes":[]}}]}}}}}`), nil
		}
		return nil, fmt.Errorf("too many calls")
	}
	var out bytes.Buffer
	if err := writeThreads(fetch, &out, "o", "r", 123); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || calls != 3 {
		t.Fatalf("unexpected output: %s", out.String())
	}
	var thread reviewThread
	if err := json.Unmarshal([]byte(lines[0]), &thread); err != nil || len(thread.Comments.Nodes) != 2 {
		t.Fatalf("comments: %s %v", lines[0], err)
	}
}
func TestThreadsRejectRepeatingCursor(t *testing.T) {
	fetch := func(string, ...string) ([]byte, error) {
		return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"x"},"nodes":[]}}}}}`), nil
	}
	if err := writeThreads(fetch, &bytes.Buffer{}, "o", "r", 1); err == nil {
		t.Fatal("accepted repeating cursor")
	}
}

func TestThreadsWithRealGH(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("invalid variables: %v", err)
		}
		vars := payload.Variables
		if payload.Query != threadsQuery {
			t.Error("unexpected query")
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			if vars["owner"] != "o" || vars["name"] != "r" || vars["number"] != float64(123) || vars["fetchThreads"] != true || vars["fetchComments"] != false {
				t.Errorf("unexpected repository variables %v", vars)
			}
			io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"t1"},"nodes":[{"id":"thread1","path":"a.go","comments":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[{"body":"first"}]}}]}}}}}`)
		case 2:
			if vars["id"] != "thread1" || vars["commentsCursor"] != "c1" || vars["fetchThreads"] != false || vars["fetchComments"] != true {
				t.Errorf("unexpected comment variables %v", vars)
			}
			io.WriteString(w, `{"data":{"node":{"comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"body":"second"}]}}}}`)
		case 3:
			if vars["cursor"] != "t1" {
				t.Errorf("unexpected thread cursor %v", vars)
			}
			io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`)
		default:
			t.Error("too many requests")
			http.Error(w, "too many", 500)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	cert := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + dir, "GH_CONFIG_DIR=" + dir, "GH_HOST=" + strings.TrimPrefix(server.URL, "https://"), "GH_ENTERPRISE_TOKEN=test-token", "GH_PROMPT_DISABLED=1", "SSL_CERT_FILE=" + cert, "NO_PROXY=*", "PATH=" + os.Getenv("PATH")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fetch := func(query string, fields ...string) ([]byte, error) {
		args, err := threadQueryArgs(query, fields...)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, gh, args...)
		cmd.Env = env
		return cmd.Output()
	}
	var out bytes.Buffer
	if err := writeThreads(fetch, &out, "o", "r", 123); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || !strings.Contains(out.String(), "first") || !strings.Contains(out.String(), "second") {
		t.Fatalf("%d calls: %q", calls.Load(), out.String())
	}
}
func TestThreadsRejectMissingPageInfo(t *testing.T) {
	fetch := func(string, ...string) ([]byte, error) {
		return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`), nil
	}
	if err := writeThreads(fetch, &bytes.Buffer{}, "o", "r", 1); err == nil {
		t.Fatal("missing pageInfo accepted")
	}
}

func TestThreadQueryRejectsAgentQueryAndInvalidNumbers(t *testing.T) {
	if _, err := threadQueryArgs(`query {viewer{login}}`, "owner=o"); err == nil {
		t.Fatal("accepted non-fixed query")
	}
	for _, value := range []string{"0", "-1", "+1", "01", "not-a-number", "99999999999999999999999"} {
		if _, err := parseThreadArgs([]string{value}, "o/r", []string{"o/*"}); err == nil {
			t.Errorf("accepted PR number %q", value)
		}
	}
}

func TestOutdatedThreadIncludesOriginalLocationAndCommentDetails(t *testing.T) {
	for _, field := range []string{"originalLine", "startLine", "originalStartLine", "diffSide", "subjectType", "url", "diffHunk"} {
		if !strings.Contains(threadsQuery, field) {
			t.Fatalf("fixed query missing %s", field)
		}
	}
	fetch := func(query string, fields ...string) ([]byte, error) {
		if query != threadsQuery {
			return nil, fmt.Errorf("unexpected query")
		}
		return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"T","isOutdated":true,"path":"file.go","line":null,"originalLine":42,"startLine":null,"originalStartLine":40,"diffSide":"RIGHT","subjectType":"LINE","comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"body":"note","url":"https://github.com/o/r/pull/1#discussion_r1","diffHunk":"@@ -1 +1 @@"}]}}]}}}}}`), nil
	}
	var out bytes.Buffer
	if err := writeThreads(fetch, &out, "o", "r", 1); err != nil {
		t.Fatal(err)
	}
	var thread struct {
		OriginalLine      *int   `json:"originalLine"`
		OriginalStartLine *int   `json:"originalStartLine"`
		DiffSide          string `json:"diffSide"`
		SubjectType       string `json:"subjectType"`
		Comments          struct {
			Nodes []struct {
				URL      string `json:"url"`
				DiffHunk string `json:"diffHunk"`
			} `json:"nodes"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.OriginalLine == nil || *thread.OriginalLine != 42 || thread.OriginalStartLine == nil || *thread.OriginalStartLine != 40 || thread.DiffSide != "RIGHT" || thread.SubjectType != "LINE" || len(thread.Comments.Nodes) != 1 || thread.Comments.Nodes[0].URL == "" || thread.Comments.Nodes[0].DiffHunk == "" {
		t.Fatalf("lost outdated thread details: %+v", thread)
	}
}
