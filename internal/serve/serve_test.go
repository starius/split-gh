package serve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"split-gh/internal/wire"
)

const testPassword = "correct-horse-battery"

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// testDaemon is gh-auth's side: a home directory with a config whose qubes
// are given as JSON, a token, and a password per qube in ~/NAME.password.
type testDaemon struct {
	home  string
	out   bytes.Buffer // the terminal; read it after closing the servers
	qubes map[string]*qube
	urls  map[string]string
	close func()
}

func newTestDaemon(t *testing.T, qubesJSON string) *testDaemon {
	t.Helper()
	d := &testDaemon{home: t.TempDir(), qubes: map[string]*qube{}, urls: map[string]string{}}
	writeFile(t, filepath.Join(d.home, ".config/split-gh/config.json"),
		`{"qubes":`+qubesJSON+`,"read_token_file":"~/token"}`)
	writeFile(t, filepath.Join(d.home, "token"), "test-token\n")
	var conf map[string]json.RawMessage
	if err := json.Unmarshal([]byte(qubesJSON), &conf); err != nil {
		t.Fatal(err)
	}
	for name := range conf {
		writeFile(t, filepath.Join(d.home, name+".password"), testPassword+"-"+name+"\n")
	}
	qubes, err := load(d.home, &d.out)
	if err != nil {
		t.Fatal(err)
	}
	// Tests never reach GitHub; git tests set their own.
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to GitHub: %s %s", r.Method, r.URL)
		http.Error(w, "test", http.StatusTeapot)
	}))
	t.Cleanup(github.Close)
	qubes[0].github, qubes[0].client = github.URL, github.Client()
	var servers []*httptest.Server
	for _, q := range qubes {
		s := httptest.NewServer(q)
		servers = append(servers, s)
		d.qubes[q.name], d.urls[q.name] = q, s.URL
	}
	d.close = func() {
		for _, s := range servers {
			s.Close()
		}
	}
	t.Cleanup(d.close)
	return d
}

// qubeJSON is the config of a qube with the given port and read_repos.
func qubeJSON(name string, port int, repos ...string) string {
	b, _ := json.Marshal(repos)
	return fmt.Sprintf(`%q:{"port":%d,"password_file":"~/%s.password","read_repos":%s}`, name, port, name, b)
}

// post sends a body to the qube's /gh with its password.
func (d *testDaemon) post(t *testing.T, qube, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", d.urls[qube]+"/gh", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testPassword+"-"+qube)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type answer struct {
	status, code   int
	stdout, stderr string
}

// gh sends a request as qube and reads the answer. A refusal's text is in
// stderr.
func (d *testDaemon) gh(t *testing.T, qube string, q wire.Request) answer {
	t.Helper()
	q.V = 1
	b, _ := json.Marshal(q)
	resp := d.post(t, qube, string(b))
	defer resp.Body.Close()
	a := answer{status: resp.StatusCode, code: -1}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		a.stderr = string(b)
		return a
	}
	var stdout, stderr bytes.Buffer
	code, err := wire.Receive(resp.Body, &stdout, &stderr)
	if err != nil {
		t.Fatalf("broken answer: %v", err)
	}
	a.code, a.stdout, a.stderr = code, stdout.String(), stderr.String()
	return a
}

func TestLoadChecksTheConfig(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
	}{
		{"no qubes", `{"qubes":{},"read_token_file":"~/token"}`, "no qubes"},
		{"no port", `{"qubes":{"ai":{"password_file":"~/pw"}},"read_token_file":"~/token"}`, "port"},
		{"same port", `{"qubes":{"ai":{"port":8401,"password_file":"~/pw"},"ai2":{"port":8401,"password_file":"~/pw"}},"read_token_file":"~/token"}`, "same port"},
		{"no password", `{"qubes":{"ai":{"port":8401}},"read_token_file":"~/token"}`, "password_file"},
		{"short password", `{"qubes":{"ai":{"port":8401,"password_file":"~/short"}},"read_token_file":"~/token"}`, "at least 16"},
		{"no token", `{"qubes":{"ai":{"port":8401,"password_file":"~/pw"}},"read_token_file":"~/missing"}`, "read_token_file"},
		{"typo", `{"qubes":{"ai":{"port":8401,"pasword_file":"~/pw"}},"read_token_file":"~/token"}`, "unknown field"},
	} {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, ".config/split-gh/config.json"), tc.config)
		writeFile(t, filepath.Join(home, "token"), "test-token")
		writeFile(t, filepath.Join(home, "pw"), testPassword)
		writeFile(t, filepath.Join(home, "short"), "too short\n")
		if _, err := load(home, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestPasswordAndLimits(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	marker := filepath.Join(bin, "ran")
	writeFile(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho x >> "+marker+"\n")
	os.Chmod(filepath.Join(bin, "gh"), 0700)
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r")+","+qubeJSON("ai2", 8402, "o/r")+"}")
	request := `{"v":1,"repo":"o/r","argv":["pr","view","1"]}`
	for _, header := range []string{"", "Bearer", "Bearer wrong-password-wrong", "Basic " + testPassword + "-ai",
		"Bearer " + testPassword + "-ai2", "Bearer " + testPassword + "-ai-"} {
		for _, path := range []string{"/gh", "/o/r/info/refs?service=git-upload-pack"} {
			req, _ := http.NewRequest("POST", d.urls["ai"]+path, strings.NewReader(request))
			if path != "/gh" {
				req.Method = "GET"
			}
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "wrong password") {
				t.Errorf("%s with %q: %d %q", path, header, resp.StatusCode, b)
			}
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("gh ran without the password")
	}

	// Over the limits: 429, which git and the shim show as an error.
	d.qubes["ai"].gh = newLimiter(1, 1)
	if a := d.gh(t, "ai", wire.Request{Repo: "o/r", Argv: []string{"pr", "view", "1"}}); a.status != http.StatusOK {
		t.Fatalf("first request: %+v", a)
	}
	if a := d.gh(t, "ai", wire.Request{Repo: "o/r", Argv: []string{"pr", "view", "1"}}); a.status != http.StatusTooManyRequests || !strings.Contains(a.stderr, "rate limit") {
		t.Fatalf("second request: %+v", a)
	}
	if a := d.gh(t, "ai2", wire.Request{Repo: "o/r", Argv: []string{"pr", "view", "1"}}); a.status != http.StatusOK {
		t.Fatalf("another qube's limit was used: %+v", a)
	}
	d.close()
	if !strings.Contains(d.out.String(), "ai: refused a request with a wrong password") {
		t.Fatalf("terminal:\n%s", d.out.String())
	}
}

func TestTerminalLines(t *testing.T) {
	q := &qube{daemon: &daemon{}, name: "ai"}
	var out bytes.Buffer
	q.out = &out
	q.printf("gh %s", show([]string{"pr", "view", "1 2", "\x1b[2J\u202e", ""}))
	q.printf("%s", strings.Repeat("x", 1000))
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	if !strings.HasSuffix(lines[0], ` ai: gh pr view "1 2" <U+001B>[2J<U+202E> ""`) {
		t.Errorf("command line: %q", lines[0])
	}
	if len(lines[1]) > 520 || !strings.HasSuffix(lines[1], "...") {
		t.Errorf("long line not cut: %d bytes", len(lines[1]))
	}
	for _, c := range out.String() {
		if c != '\n' && (c < 0x20 || c > 0x7e) {
			t.Fatalf("terminal got %U", c)
		}
	}
}

// smallBuffers gives the server's connections a small send buffer, so a
// client that stops reading soon blocks the server's writes.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

// A client that stops reading must not keep its slot: here a refusal that
// echoes a long field name blocks, and only a write deadline ends it.
func TestStalledClientFreesItsSlot(t *testing.T) {
	old := writeTimeout
	writeTimeout = time.Second
	t.Cleanup(func() { writeTimeout = old })
	fakeGH(t, "#!/bin/sh\necho ok\n")
	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r")+"}")
	q := d.qubes["ai"]
	q.gh = newLimiter(1000, 1)
	srv := httptest.NewUnstartedServer(q)
	srv.Listener = smallBuffers{srv.Listener}
	srv.Start()
	defer srv.Close()

	dialer := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
	}}
	conn, err := dialer.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"v":1,"` + strings.Repeat("x", 60000) + `":1}`
	fmt.Fprintf(conn, "POST /gh HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s-ai\r\nContent-Length: %d\r\n\r\n%s", testPassword, len(body), body)

	// The stalled request holds the only slot, then loses it.
	held := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ := json.Marshal(wire.Request{V: 1, Repo: "o/r", Argv: []string{"pr", "view", "1"}})
		req, _ := http.NewRequest("POST", srv.URL+"/gh", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+testPassword+"-ai")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			held = true
		case resp.StatusCode == http.StatusOK && held:
			return
		}
	}
	t.Fatalf("the stalled request kept its slot (seen held: %v)", held)
}
