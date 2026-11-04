package serve

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"split-gh/internal/wire"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadIntegration(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("QREXEC_REMOTE_DOMAIN", "ai")
	os.MkdirAll(filepath.Join(home, ".config/split-gh"), 0700)
	os.WriteFile(filepath.Join(home, ".config/split-gh/config.json"), []byte(`{"qubes":{"ai":{"read_repos":["o/*"]}},"read_token_file":"~/token"}`), 0600)
	os.WriteFile(filepath.Join(home, "token"), []byte("secret"), 0600)
	os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\ncase \"$*\" in 'run list'*) printf '[{\"databaseId\":42}]\\n' ;; *reviewThreads*) printf '%s\\n' '{\"data\":{\"repository\":{\"pullRequest\":{\"reviewThreads\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[{\"id\":\"T\",\"path\":\"file.go\",\"comments\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[]}}]}}}}}' ;; *) printf '%s\\n' \"$@\";; esac\n"), 0700)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"pr", "view", "123", "--comments"}, 0, "--repo=o/r"},
		{[]string{"api", "repos/o/r/pulls/123/comments"}, 0, "--method=GET"},
		{[]string{"run", "view", "--log-failed"}, 0, "42"},
		{[]string{"pr", "threads", "123"}, 0, "file.go"},
		{[]string{"api", "-X", "POST", "repos/o/r"}, 1, "only GET"},
		{[]string{"pr", "comment", "123"}, 1, "not allowed"},
	} {
		var request, response, stdout, stderr bytes.Buffer
		json.NewEncoder(&request).Encode(wire.Request{V: 1, Repo: "o/r", Argv: tc.args})
		Read(&request, &response)
		code, err := wire.Receive(&response, &stdout, &stderr)
		if err != nil || code != tc.code || !strings.Contains(stdout.String()+stderr.String(), tc.want) {
			t.Errorf("%v: code=%d err=%v stdout=%q stderr=%q", tc.args, code, err, stdout.String(), stderr.String())
		}
	}
	data, err := os.ReadFile(filepath.Join(home, ".local/state/split-gh/log.jsonl"))
	if err != nil || len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 12 {
		t.Fatalf("missing audit records: %q %v", data, err)
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

func TestReadHeaderTimeout(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	_, err := readHeader(r, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
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

func TestReadIsolationAndCallerPolicy(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("QREXEC_REMOTE_DOMAIN", "ai")
	t.Setenv("GH_HOST", "attacker.invalid")
	t.Setenv("HTTPS_PROXY", "http://attacker.invalid")
	t.Setenv("GH_DEBUG", "api")
	if err := os.MkdirAll(filepath.Join(home, ".config/split-gh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/split-gh/config.json"), []byte(`{"qubes":{"ai":{"read_repos":["o/r"]},"other":{"read_repos":["other/r"]}},"read_token_file":"~/token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "token"), []byte("test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
[ "$GH_TOKEN" = test-token ] || exit 20
[ "$HOME" = "$PWD" ] && [ "$GH_CONFIG_DIR" = "$PWD" ] || exit 21
[ "$GH_PROMPT_DISABLED" = 1 ] && [ "$GH_NO_UPDATE_NOTIFIER" = 1 ] || exit 22
[ "$GH_PAGER" = cat ] && [ "$NO_COLOR" = 1 ] || exit 23
[ -z "$GH_HOST$HTTPS_PROXY$GH_DEBUG" ] || exit 24
printf '%s' "$HOME"
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	invoke := func(caller, repo string) (int, string, string) {
		t.Helper()
		t.Setenv("QREXEC_REMOTE_DOMAIN", caller)
		var req, response, out, errout bytes.Buffer
		json.NewEncoder(&req).Encode(wire.Request{V: 1, Repo: repo, Argv: []string{"pr", "view", "1"}})
		Read(&req, &response)
		code, err := wire.Receive(&response, &out, &errout)
		if err != nil {
			t.Fatal(err)
		}
		return code, out.String(), errout.String()
	}
	code, tmp, errout := invoke("ai", "o/r")
	if code != 0 || errout != "" || tmp == home || tmp == "" {
		t.Fatalf("isolation failure: %d %q %q", code, tmp, errout)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temporary home not removed: %v", err)
	}
	for _, caller := range []string{"other", "unknown", ""} {
		code, out, _ := invoke(caller, "o/r")
		if code == 0 || out != "" {
			t.Fatalf("caller %q was not rejected", caller)
		}
	}
	log, err := os.ReadFile(filepath.Join(home, ".local/state/split-gh/log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(log, []byte("test-token")) {
		t.Fatal("token written to audit log")
	}
}

func TestRESTWithoutCheckout(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("QREXEC_REMOTE_DOMAIN", "ai")
	if err := os.MkdirAll(filepath.Join(home, ".config/split-gh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/split-gh/config.json"), []byte(`{"qubes":{"ai":{"read_repos":["o/r"]}},"read_token_file":"~/token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "token"), []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		ok   bool
	}{{"repos/o/r/pulls/1", true}, {"meta", true}, {"repos/other/r", false}, {"user/emails", false}} {
		var in, response, stdout, stderr bytes.Buffer
		json.NewEncoder(&in).Encode(wire.Request{V: 1, Argv: []string{"api", tc.path}})
		Read(&in, &response)
		code, err := wire.Receive(&response, &stdout, &stderr)
		if err != nil || (code == 0) != tc.ok {
			t.Errorf("%s: %d %q %v", tc.path, code, stderr.String(), err)
		}
	}
}

func TestReadRefusesIfAuditUnavailable(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("QREXEC_REMOTE_DOMAIN", "ai")
	if err := os.MkdirAll(filepath.Join(home, ".config/split-gh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/split-gh/config.json"), []byte(`{"qubes":{"ai":{"read_repos":["o/r"]}},"read_token_file":"~/token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "token"), []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".local"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local/state"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "invoked")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf x > "+strconv.Quote(marker)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var in, response, stdout, stderr bytes.Buffer
	json.NewEncoder(&in).Encode(wire.Request{V: 1, Repo: "o/r", Argv: []string{"pr", "view", "1"}})
	Read(&in, &response)
	code, err := wire.Receive(&response, &stdout, &stderr)
	if err != nil || code == 0 || !strings.Contains(stderr.String(), "audit log unavailable") {
		t.Fatalf("%d %q %v", code, stderr.String(), err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gh unexpectedly invoked: %v", err)
	}
}

func TestOutputDeadlineStopsStalledReader(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	setOutputDeadline(w, 20*time.Millisecond)
	_, err = w.Write(make([]byte, 1<<20))
	if !os.IsTimeout(err) {
		t.Fatalf("expected blocked pipe write deadline, got %v", err)
	}
}

func TestConcurrentAuditCorrelation(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("QREXEC_REMOTE_DOMAIN", "ai")
	if err := os.MkdirAll(filepath.Join(home, ".config/split-gh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/split-gh/config.json"), []byte(`{"qubes":{"ai":{"read_repos":["o/r"]}},"read_token_file":"~/token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "token"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf ok\\n\n"), 0700); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var group sync.WaitGroup
	for i := 0; i < n; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			var req, response, out, stderr bytes.Buffer
			json.NewEncoder(&req).Encode(wire.Request{V: 1, Repo: "o/r", Argv: []string{"pr", "view", "1"}})
			Read(&req, &response)
			code, err := wire.Receive(&response, &out, &stderr)
			if err != nil || code != 0 {
				t.Errorf("request %d %q %v", code, stderr.String(), err)
			}
		}()
	}
	group.Wait()
	raw, err := os.ReadFile(filepath.Join(home, ".local/state/split-gh/log.jsonl"))
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
	if len(events) != n {
		t.Fatalf("got %d request IDs", len(events))
	}
	for id, phases := range events {
		if phases["start"] != 1 || phases["finish"] != 1 {
			t.Errorf("%s: %v", id, phases)
		}
	}
}

func TestForgedCallerAndMalformedHeadersNeverRunGH(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	if err := os.MkdirAll(filepath.Join(home, ".config/split-gh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/split-gh/config.json"), []byte(`{"qubes":{"ai":{"read_repos":["o/r"]},"other":{"read_repos":["other/r"]}},"read_token_file":"~/token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "token"), []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "invoked")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf x > "+strconv.Quote(marker)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ caller, header string }{
		{"other", `{"v":1,"repo":"o/r","argv":["pr","view","1"],"caller":"ai"}` + "\n"},
		{"other", `{"v":1,"repo":"o/r","argv":["pr","view","1"]}` + "\n"},
		{"ai", `{"v":1,"repo":"o/r","argv":["api","-XPOST","repos/o/r"]}` + "\n"},
		{"ai", `{"v":1,"repo":"o/r","argv":["pr","comment","1"]}` + "\n"},
		{"ai", `{"v":1,"repo":"o/r","argv":["api","graphql","--paginate","-f","query=query($endCursor:String!){repository(owner:$endCursor,name:\"r\"){name}}","-f","endCursor=o"]}` + "\n"},
		{"ai", `{"v":1,"repo":"o/r","argv":["pr","view","1"]} {}` + "\n"},
		{"ai", strings.Repeat("x", wire.MaxHeader+1) + "\n"},
	} {
		t.Setenv("QREXEC_REMOTE_DOMAIN", tc.caller)
		var response, stdout, stderr bytes.Buffer
		Read(strings.NewReader(tc.header), &response)
		code, err := wire.Receive(&response, &stdout, &stderr)
		if err != nil || code == 0 || stdout.Len() != 0 {
			t.Errorf("forged header accepted: caller=%q status=%d error=%v", tc.caller, code, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gh unexpectedly ran: %v", err)
	}
}
