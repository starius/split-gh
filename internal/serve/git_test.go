package serve

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestGitProxy clones through the proxy from git http-backend, standing in
// for GitHub, which also accepts pushes: only the proxy stops them.
func TestGitProxy(t *testing.T) {
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not installed")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	root, seed, ai := t.TempDir(), t.TempDir(), t.TempDir()
	env := []string{"HOME=" + ai, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(dir string, args ...string) {
		t.Helper()
		if out, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	must(seed, "init", "--quiet")
	writeFile(t, filepath.Join(seed, "README"), "hello\n")
	must(seed, "add", "README")
	must(seed, "-c", "user.name=U", "-c", "user.email=u@x", "commit", "--quiet", "-m", "Initial")
	for _, repo := range []string{"o/r.git", "o/secret.git"} {
		must(seed, "init", "--quiet", "--bare", "--initial-branch=main", filepath.Join(root, repo))
		must(seed, "push", "--quiet", filepath.Join(root, repo), "HEAD:refs/heads/main")
		must(filepath.Join(root, repo), "config", "http.receivepack", "true")
	}

	var mu sync.Mutex
	var seen []string
	backendHandler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, Stderr: io.Discard}
	token := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:test-token"))
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, strings.Join([]string{r.Method, r.URL.String(), r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Header.Get("Git-Protocol")}, " | "))
		mu.Unlock()
		if r.Header.Get("Authorization") != token {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		backendHandler.ServeHTTP(w, r)
	}))
	defer github.Close()

	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r", "o/missing")+"}")
	d.qubes["ai"].github, d.qubes["ai"].client = github.URL, github.Client()
	proxy := d.urls["ai"] + "/"
	writeFile(t, filepath.Join(ai, ".gitconfig"), "[url \""+proxy+"\"]\n\tinsteadOf = https://github.com/\n"+
		"[http \""+proxy+"\"]\n\textraHeader = Authorization: Bearer "+testPassword+"-ai\n"+
		"[user]\n\tname = Agent\n\temail = agent@ai\n")

	for _, v := range []string{"0", "2"} {
		must(ai, "-c", "protocol.version="+v, "clone", "--quiet", "https://github.com/o/r", "r"+v)
		if out, _ := git(filepath.Join(ai, "r"+v), "log", "--format=%s"); out != "Initial\n" {
			t.Fatalf("protocol v%s clone has %q", v, out)
		}
		must(filepath.Join(ai, "r"+v), "-c", "protocol.version="+v, "fetch", "--quiet", "origin")
	}
	clone := filepath.Join(ai, "r2")
	must(clone, "commit", "--quiet", "--allow-empty", "-m", "Agent's")
	for _, tc := range []struct {
		name, dir string
		args      []string
		want      string
	}{
		{"push", clone, []string{"push", "origin", "HEAD:refs/heads/x"}, "remote: This qube can't push to GitHub"},
		{"repository not in read_repos", ai, []string{"clone", "https://github.com/o/secret"}, "remote: split-gh: this qube may not read o/secret"},
		{"missing repository", ai, []string{"clone", "https://github.com/o/missing"}, "remote: split-gh: GitHub has no repository o/missing"},
		{"wrong password", ai, []string{"-c", "http." + proxy + ".extraHeader=", "-c", "http." + proxy + ".extraHeader=Authorization: Bearer wrong",
			"clone", "https://github.com/o/r", "wrong"}, "remote: split-gh: wrong password"},
	} {
		out, err := git(tc.dir, tc.args...)
		if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, "prompt") {
			t.Errorf("%s: %v\n%s", tc.name, err, out)
		}
	}
	if out, _ := git(filepath.Join(root, "o/r.git"), "for-each-ref", "--format=%(refname)"); out != "refs/heads/main\n" {
		t.Fatalf("GitHub's refs changed: %q", out)
	}

	mu.Lock()
	defer mu.Unlock()
	v2 := false
	for _, line := range seen {
		if strings.Contains(line, "receive-pack") || strings.Contains(line, "secret") || strings.Contains(line, "Bearer") ||
			!strings.Contains(line, " | "+token+" | git/") {
			t.Errorf("GitHub got: %s", line)
		}
		v2 = v2 || strings.HasSuffix(line, "version=2")
	}
	if !v2 {
		t.Error("protocol v2 didn't reach GitHub")
	}

	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/o/r/info/refs", http.StatusBadRequest}, // git's old dumb protocol
		{"POST", "/o/r/info/refs?service=git-upload-pack", http.StatusBadRequest},
		{"GET", "/o/r/git-upload-pack", http.StatusBadRequest},
		{"GET", "/o/r/info/refs?service=git-receive-pack", http.StatusForbidden},
		{"GET", "/o/r/info/lfs/objects", http.StatusNotFound},
		{"GET", "/o/../info/refs?service=git-upload-pack", http.StatusForbidden},
		{"GET", "/o/r/HEAD", http.StatusNotFound},
	} {
		req, _ := http.NewRequest(tc.method, d.urls["ai"]+tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+testPassword+"-ai")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s %s: %d", tc.method, tc.path, resp.StatusCode)
		}
	}

	d.close()
	for _, want := range []string{"ai: git fetch o/r info/refs -> ", "ai: git push to o/r -> refused"} {
		if !strings.Contains(d.out.String(), want) {
			t.Errorf("terminal has no %q:\n%s", want, d.out.String())
		}
	}
	log, _ := os.ReadFile(filepath.Join(d.home, ".local/state/split-gh/log.jsonl"))
	if !strings.Contains(string(log), `"op":"git-upload-pack"`) || !strings.Contains(string(log), `"outcome":"refused: This qube can't push to GitHub; split-gh only fetches."`) ||
		strings.Contains(string(log), "test-token") || strings.Contains(string(log), testPassword) {
		t.Errorf("audit log:\n%s", log)
	}
}
