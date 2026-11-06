package client

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"split-gh/internal/wire"
)

// fakeServe stands in for split-gh serve. It records the requests and
// answers each with the given status and body; a 200 answer is exit 0.
type fakeServe struct {
	mu       sync.Mutex
	requests []wire.Request
	auth     []string
	status   int
	body     string
}

func newFakeServe(t *testing.T) *fakeServe {
	t.Helper()
	f := &fakeServe{status: http.StatusOK}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		q, err := wire.DecodeRequest(b)
		if err != nil || r.URL.Path != "/gh" || r.Method != "POST" {
			t.Errorf("bad request %s %s: %v", r.Method, r.URL, err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, q)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if f.status != http.StatusOK {
			http.Error(w, f.body, f.status)
			return
		}
		wire.Frame(w, 'x', []byte{0})
	}))
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	t.Setenv("SPLIT_GH_PORT", u.Port())
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config/split-gh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/split-gh/password"), []byte("the-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeServe) last(t *testing.T) wire.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request")
	}
	return f.requests[len(f.requests)-1]
}

func TestShimSendsRequest(t *testing.T) {
	dir := t.TempDir()
	fakeGit := "#!/bin/sh\ncase \"$*\" in\n'remote get-url -- origin') echo https://github.com/owner/project.git;;\n'branch --show-current') echo feature/foo;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(fakeGit), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GH_REPO", "")
	f := newFakeServe(t)
	if code := Main([]string{"pr", "view", "123", "--comments"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	q := f.last(t)
	if q.Repo != "owner/project" || q.Branch != "feature/foo" || strings.Join(q.Argv, " ") != "pr view 123 --comments" || q.V != 1 {
		t.Fatalf("unexpected request: %+v", q)
	}
	if f.auth[0] != "Bearer the-password" {
		t.Fatalf("Authorization: %q", f.auth[0])
	}
	if code := Main([]string{"pr", "list", "--search", "--repo=not-an-option"}); code != 0 {
		t.Fatal(code)
	}
	if q := f.last(t); q.Repo != "owner/project" {
		t.Fatalf("flag value treated as repository: %+v", q)
	}
	t.Setenv("GH_REPO", "github.com/env/project")
	if code := Main([]string{"pr", "view", "1"}); code != 0 {
		t.Fatal(code)
	}
	if q := f.last(t); q.Repo != "env/project" {
		t.Fatalf("GH_REPO ignored: %+v", q)
	}
	if code := Main([]string{"pr", "view", "1", "-R", "github.com/override/repo"}); code != 0 {
		t.Fatal(code)
	}
	if q := f.last(t); q.Repo != "override/repo" {
		t.Fatalf("-R ignored: %+v", q)
	}
}

// stderrOf runs f and returns what it wrote to stderr.
func stderrOf(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	os.Stderr = old
	w.Close()
	return <-done
}

func TestShimExplainsRefusalsAndOutages(t *testing.T) {
	t.Setenv("GH_REPO", "o/r")
	t.Setenv("PATH", t.TempDir())
	f := newFakeServe(t)
	run := func() (int, string) {
		code := 0
		out := stderrOf(t, func() { code = Main([]string{"pr", "view", "1"}) })
		return code, out
	}

	f.status, f.body = http.StatusForbidden, `command "x" is not allowed`
	if code, out := run(); code != 1 || !strings.Contains(out, `gh: command "x" is not allowed`) || !strings.Contains(out, About) {
		t.Errorf("refusal: %d %q", code, out)
	}
	f.status, f.body = http.StatusTooManyRequests, "split-gh: rate limit exceeded"
	if code, out := run(); code != 1 || !strings.Contains(out, "rate limit") || strings.Contains(out, About) {
		t.Errorf("rate limit: %d %q", code, out)
	}
	out := stderrOf(t, func() { Main([]string{"pr", "comment", "1", "--body", "x"}) })
	if !strings.Contains(out, "only the user") || !strings.Contains(out, About) {
		t.Errorf("local refusal: %q", out)
	}

	// A connection that closes without an answer: serve isn't running in
	// gh-auth, and qvm-connect-tcp's socat closes the connection.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	t.Setenv("SPLIT_GH_PORT", port)
	if code, out := run(); code != 1 || !strings.Contains(out, "split-gh serve isn't running in gh-auth") {
		t.Errorf("no serve: %d %q", code, out)
	}
	l.Close()
	if code, out := run(); code != 1 || !strings.Contains(out, "nothing listens on port "+port) {
		t.Errorf("no forwarding: %d %q", code, out)
	}
	t.Setenv("HOME", t.TempDir())
	if code, out := run(); code != 1 || !strings.Contains(out, "isn't set up in this qube") {
		t.Errorf("no password: %d %q", code, out)
	}
}

func TestAboutRequests(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		about bool
	}{
		{nil, true}, {[]string{"help"}, true}, {[]string{"--help"}, true}, {[]string{"--version"}, true},
		{[]string{"auth", "status"}, true}, {[]string{"auth", "token"}, false}, {[]string{"pr", "view"}, false},
	} {
		if got := aboutRequest(tc.args); got != tc.about {
			t.Errorf("%v: %v", tc.args, got)
		}
	}
	for _, line := range strings.Split(About, "\n") {
		if len(line) > 80 {
			t.Errorf("About has a line over 80 columns: %q", line)
		}
	}
}

func TestRemoteURLs(t *testing.T) {
	for _, tc := range []struct{ url, repo string }{
		{"https://github.com/o/r.git", "o/r"}, {"git@github.com:o/r.git", "o/r"}, {"ssh://git@github.com/o/r", "o/r"},
		{"split-gh::https://github.com/o/r.git", "o/r"}, {"split-gh::https://github.com/o/r", "o/r"},
	} {
		m := remoteRE.FindStringSubmatch(tc.url)
		if m == nil || m[1] != tc.repo {
			t.Errorf("%q: %q", tc.url, m)
		}
	}
	for _, u := range []string{"http://example.com/o/r", "split-gh::https://github.com/o/r/x", "http://127.0.0.1:8401/o/r"} {
		if remoteRE.MatchString(u) {
			t.Errorf("%q matched", u)
		}
	}
}

func TestLocalWriteRefusal(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		refuse bool
	}{
		{[]string{"pr", "comment", "1"}, true}, {[]string{"-R", "o/r", "pr", "comment", "1"}, true},
		{[]string{"api", "-X", "POST", "repos/o/r"}, true}, {[]string{"api", "-XPOST", "repos/o/r"}, true},
		{[]string{"api", "--method=post", "repos/o/r"}, true}, {[]string{"api", "--input", "-", "repos/o/r"}, true},
		{[]string{"api", "-X", "GET", "repos/o/r"}, false}, {[]string{"api", "repos/o/r", "-f", "query=-XPOST"}, false},
		{[]string{"pr", "view", "1"}, false},
	} {
		if got := locallyRefusedWrite(tc.args); got != tc.refuse {
			t.Errorf("%v: %v", tc.args, got)
		}
	}
}

func TestInferRepoHonorsGHResolvedBase(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "missing-global-config"))
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "https://github.com/fork/project.git")
	run("remote", "add", "upstream", "https://github.com/upstream/project.git")
	run("remote", "add", "custom.remote", "https://github.com/custom/project.git")
	if got, err := inferRepo(); err != nil || got != "upstream/project" {
		t.Fatalf("fallback: %q %v", got, err)
	}
	run("config", "remote.custom.remote.gh-resolved", "base")
	if got, err := inferRepo(); err != nil || got != "custom/project" {
		t.Fatalf("configured base: %q %v", got, err)
	}
	run("config", "--add", "remote.custom.remote.gh-resolved", "other/target")
	if got, err := inferRepo(); err != nil || got != "other/target" {
		t.Fatalf("resolved repository: %q %v", got, err)
	}
	run("config", "--replace-all", "remote.custom.remote.gh-resolved", "invalid")
	if _, err := inferRepo(); err == nil {
		t.Fatal("invalid gh resolution fell back silently")
	}
	f := newFakeServe(t)
	t.Setenv("PATH", filepath.Dir(gitPath))
	t.Setenv("GH_REPO", "")
	if code := Main([]string{"pr", "view", "1", "-R", "override/repo"}); code != 0 {
		t.Fatalf("explicit override failed: %d", code)
	}
	if q := f.last(t); q.Repo != "override/repo" {
		t.Fatalf("wrong override: %+v", q)
	}
	run("config", "--unset", "remote.custom.remote.gh-resolved")
	run("remote", "remove", "upstream")
	if got, err := inferRepo(); err != nil || got != "fork/project" {
		t.Fatalf("origin fallback: %q %v", got, err)
	}
	// git applies insteadOf to the URLs it reports, as the agent qube's
	// config has it for git-remote-split-gh.
	run("config", "url.split-gh::https://github.com/.insteadOf", "https://github.com/")
	if got, err := inferRepo(); err != nil || got != "fork/project" {
		t.Fatalf("with insteadOf: %q %v", got, err)
	}
}

func TestRepoFromAPIPathWithQuery(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"api", "repos/o/r?state=open"}, "o/r"},
		{[]string{"api", "-X", "GET", "repos/o/r/issues?state=closed"}, "o/r"},
		{[]string{"api", "graphql?query=x"}, ""},
		{[]string{"api", "repos/../r?x=y"}, ""},
	} {
		if got := repoFromAPIArgs(tc.args); got != tc.want {
			t.Errorf("%v => %q", tc.args, got)
		}
	}
}
