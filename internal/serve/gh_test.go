package serve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"split-gh/internal/wire"
)

// fakeGH puts a gh script in an empty PATH.
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestReadIntegration(t *testing.T) {
	fakeGH(t, "#!/bin/sh\ncase \"$*\" in 'run list'*) printf '[{\"databaseId\":42}]\\n' ;; *reviewThreads*) printf '%s\\n' '{\"data\":{\"repository\":{\"pullRequest\":{\"reviewThreads\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[{\"id\":\"T\",\"path\":\"file.go\",\"comments\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[]}}]}}}}}' ;; *) printf '%s\\n' \"$@\";; esac\n")
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/*")+"}")
	for _, tc := range []struct {
		args   []string
		status int
		want   string
	}{
		{[]string{"pr", "view", "123", "--comments"}, 200, "--repo=o/r"},
		{[]string{"api", "repos/o/r/pulls/123/comments"}, 200, "--method=GET"},
		{[]string{"run", "view", "--log-failed"}, 200, "42"},
		{[]string{"pr", "threads", "123"}, 200, "file.go"},
		{[]string{"api", "-X", "POST", "repos/o/r"}, 403, "only GET"},
		{[]string{"pr", "comment", "123"}, 403, "not allowed"},
	} {
		a := d.gh(t, "ai", wire.Request{Repo: "o/r", Argv: tc.args})
		if a.status != tc.status || (a.status == 200 && a.code != 0) || !strings.Contains(a.stdout+a.stderr, tc.want) {
			t.Errorf("%v: %+v", tc.args, a)
		}
	}
	data, err := os.ReadFile(filepath.Join(d.home, ".local/state/split-gh/log.jsonl"))
	if err != nil || strings.Count(string(data), `"phase":"execute"`) != 5 || !strings.Contains(string(data), `"--repo=o/r"`) {
		t.Fatalf("missing audit records: %q %v", data, err)
	}
	d.close()
	if !strings.Contains(d.out.String(), "ai: gh pr view 123 --comments -> exit 0") ||
		!strings.Contains(d.out.String(), "ai: gh pr comment 123 -> refused: ") {
		t.Fatalf("terminal:\n%s", d.out.String())
	}
}

func TestProjectViewIntegration(t *testing.T) {
	fakeGH(t, "#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	entry := qubeJSON("ai", 8401, "o/*")
	entry = strings.TrimSuffix(entry, "}") + `,"read_projects":["example/12"]}`
	d := newTestDaemon(t, "{"+entry+"}")
	defer d.close()
	for _, tc := range []struct {
		args   []string
		status int
	}{
		{[]string{"project", "view", "12", "--owner=example", "--format=json"}, 200},
		{[]string{"project", "view", "13", "--owner=example"}, 403},
		{[]string{"project", "view", "12", "--owner=other"}, 403},
		{[]string{"project", "edit", "12", "--owner=example"}, 403},
		{[]string{"project", "view", "12", "--owner=example", "--jq=env.GH_TOKEN"}, 403},
	} {
		a := d.gh(t, "ai", wire.Request{Argv: tc.args})
		if a.status != tc.status || tc.status == 200 && (a.code != 0 || !strings.Contains(a.stdout, "--owner=example") || strings.Contains(a.stdout, "--repo")) {
			t.Errorf("%v: %+v", tc.args, a)
		}
	}
}

func TestStreamOutputLimit(t *testing.T) {
	var out bytes.Buffer
	cancelled := false
	stream := &outputStream{out: &out, cancel: func() { cancelled = true }}
	w := frameWriter{stream, 'o'}
	if _, err := w.Write(make([]byte, wire.MaxOutput-4096)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil || !cancelled || !stream.overflow {
		t.Fatal("output limit did not cancel command")
	}
}

// A client that sends headers but not the body it announced is answered
// once bodyTimeout passes.
func TestBodyTimeout(t *testing.T) {
	old := bodyTimeout
	bodyTimeout = 100 * time.Millisecond
	t.Cleanup(func() { bodyTimeout = old })
	fakeGH(t, "#!/bin/sh\nexit 0\n")
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r")+"}")
	conn, err := net.Dial("tcp", strings.TrimPrefix(d.urls["ai"], "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /gh HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s-ai\r\nContent-Length: 100\r\n\r\n{", testPassword)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	status := make([]byte, 12)
	if _, err := io.ReadFull(conn, status); err != nil || string(status) != "HTTP/1.1 400" {
		t.Fatalf("got %q %v", status, err)
	}
}

func TestNeedsLatestRun(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"run", "view", "--log-failed", "--repo=o/r"}, true},
		{[]string{"run", "view", "--job=17", "--repo=o/r"}, false},
		{[]string{"run", "view", "123", "--log-failed", "--repo=o/r"}, false},
		{[]string{"run", "list", "--repo=o/r"}, false},
	} {
		if got := needsLatestRun(tc.args); got != tc.want {
			t.Errorf("%v: got %v", tc.args, got)
		}
	}
}

func TestThreadArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"123"}, true}, {[]string{"--repo=o/r", "123"}, true}, {[]string{"123", "-R", "o/r"}, true},
		{[]string{"123", "--repo=other/r"}, false}, {[]string{"123", "--web"}, false}, {[]string{"123", "456"}, false},
	} {
		_, err := parseThreadArgs(tc.args, "o/r", []string{"o/*"})
		if (err == nil) != tc.ok {
			t.Errorf("%v: %v", tc.args, err)
		}
	}
}

func TestReadIsolationAndQubePolicy(t *testing.T) {
	t.Setenv("GH_HOST", "attacker.invalid")
	t.Setenv("HTTPS_PROXY", "http://attacker.invalid")
	t.Setenv("GH_DEBUG", "api")
	fakeGH(t, `#!/bin/sh
[ "$GH_TOKEN" = test-token ] || exit 20
[ "$HOME" = "$PWD" ] && [ "$GH_CONFIG_DIR" = "$PWD" ] || exit 21
[ "$GH_PROMPT_DISABLED" = 1 ] && [ "$GH_NO_UPDATE_NOTIFIER" = 1 ] || exit 22
[ "$GH_PAGER" = cat ] && [ "$NO_COLOR" = 1 ] || exit 23
[ -z "$GH_HOST$HTTPS_PROXY$GH_DEBUG" ] || exit 24
printf '%s' "$HOME"
`)
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r")+","+qubeJSON("other", 8402, "other/r")+"}")
	a := d.gh(t, "ai", wire.Request{Repo: "o/r", Argv: []string{"pr", "view", "1"}})
	if a.status != 200 || a.code != 0 || a.stderr != "" || a.stdout == d.home || a.stdout == "" {
		t.Fatalf("isolation failure: %+v", a)
	}
	// Each qube reads only its own repositories.
	if a := d.gh(t, "other", wire.Request{Repo: "o/r", Argv: []string{"pr", "view", "1"}}); a.status != http.StatusForbidden {
		t.Fatalf("qube other read o/r: %+v", a)
	}
	d.close() // the handlers clean up after answering
	if _, err := os.Stat(a.stdout); !os.IsNotExist(err) {
		t.Fatalf("temporary home not removed: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(d.home, ".local/state/split-gh/log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(log, []byte("test-token")) || bytes.Contains(log, []byte(testPassword)) {
		t.Fatal("secret written to audit log")
	}
	if !bytes.Contains(log, []byte(`"caller":"other"`)) {
		t.Fatalf("refusal not logged:\n%s", log)
	}
}

func TestRESTWithoutCheckout(t *testing.T) {
	fakeGH(t, "#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r")+"}")
	for _, tc := range []struct {
		path string
		ok   bool
	}{{"repos/o/r/pulls/1", true}, {"meta", true}, {"repos/other/r", false}, {"user/emails", false}} {
		a := d.gh(t, "ai", wire.Request{Argv: []string{"api", tc.path}})
		if (a.status == 200 && a.code == 0) != tc.ok {
			t.Errorf("%s: %+v", tc.path, a)
		}
	}
}

func TestReadRefusesIfAuditUnavailable(t *testing.T) {
	bin := fakeGH(t, "")
	marker := filepath.Join(bin, "invoked")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf x > "+strconv.Quote(marker)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r")+"}")
	// A directory where the log file should be: the log can't be written.
	if err := os.MkdirAll(filepath.Join(d.home, ".local/state/split-gh/log.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	a := d.gh(t, "ai", wire.Request{Repo: "o/r", Argv: []string{"pr", "view", "1"}})
	if a.status != http.StatusInternalServerError || !strings.Contains(a.stderr, "audit log unavailable") {
		t.Fatalf("%+v", a)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gh unexpectedly invoked: %v", err)
	}
}

func TestConcurrentAuditCorrelation(t *testing.T) {
	fakeGH(t, "#!/bin/sh\nprintf ok\\n\n")
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r")+"}")
	var group sync.WaitGroup
	for i := 0; i < ghSlots; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if a := d.gh(t, "ai", wire.Request{Repo: "o/r", Argv: []string{"pr", "view", "1"}}); a.status != 200 || a.code != 0 {
				t.Errorf("request: %+v", a)
			}
		}()
	}
	group.Wait()
	d.close() // the handlers write their last records after answering
	raw, err := os.ReadFile(filepath.Join(d.home, ".local/state/split-gh/log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event struct {
			ID    string `json:"request_id"`
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.ID == "" {
			t.Fatalf("missing ID: %s", line)
		}
		if events[event.ID] == nil {
			events[event.ID] = map[string]int{}
		}
		events[event.ID][event.Phase]++
	}
	if len(events) != ghSlots {
		t.Fatalf("got %d request IDs", len(events))
	}
	for id, phases := range events {
		if phases["start"] != 1 || phases["execute"] != 1 || phases["finish"] != 1 {
			t.Errorf("%s: %v", id, phases)
		}
	}
}

func TestForgedRequestsNeverRunGH(t *testing.T) {
	bin := fakeGH(t, "")
	marker := filepath.Join(bin, "invoked")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf x > "+strconv.Quote(marker)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r")+","+qubeJSON("other", 8402, "other/r")+"}")
	for _, tc := range []struct{ qube, body string }{
		{"other", `{"v":1,"repo":"o/r","argv":["pr","view","1"],"caller":"ai"}`},
		{"other", `{"v":1,"repo":"o/r","argv":["pr","view","1"]}`},
		{"ai", `{"v":1,"repo":"o/r","argv":["api","-XPOST","repos/o/r"]}`},
		{"ai", `{"v":1,"repo":"o/r","argv":["pr","comment","1"]}`},
		{"ai", `{"v":1,"repo":"o/r","argv":["api","graphql","--paginate","-f","query=query($endCursor:String!){repository(owner:$endCursor,name:\"r\"){name}}","-f","endCursor=o"]}`},
		{"ai", `{"v":1,"repo":"o/r","argv":["pr","view","1"]} {}`},
		{"ai", `{"v":2,"repo":"o/r","argv":["pr","view","1"]}`},
		{"ai", `{"v":1,"repo":"o/r","argv":["` + strings.Repeat("x", wire.MaxRequest) + `"]}`},
	} {
		resp := d.post(t, tc.qube, tc.body)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 4 {
			t.Errorf("forged request accepted: qube=%s %.80s: %d %q", tc.qube, tc.body, resp.StatusCode, b)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gh unexpectedly ran: %v", err)
	}
}
