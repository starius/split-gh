package serve

import (
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestGitProxy fetches with git as the agent qube does, through
// git-remote-split-gh: through serve for the repositories in read_repos,
// and otherwise from "GitHub" itself, without a token. git http-backend
// stands in for GitHub. It serves pub/* without a token, and accepts pushes
// to o/*, so only serve stops them.
func TestGitProxy(t *testing.T) {
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not installed")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	root, seed, ai, bin := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	// The agent qube's git runs split-gh as git-remote-split-gh.
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "git-remote-split-gh"), "split-gh/cmd/split-gh").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	git := func(env []string, dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		// No terminal: git can't ask for a password, even if it tries.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(env []string, dir string, args ...string) string {
		t.Helper()
		out, err := git(env, dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}
	plain := []string{"HOME=" + seed, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}
	must(plain, seed, "init", "--quiet")
	writeFile(t, filepath.Join(seed, "README"), "hello\n")
	must(plain, seed, "add", "README")
	must(plain, seed, "-c", "user.name=U", "-c", "user.email=u@x", "commit", "--quiet", "-m", "Initial")
	for _, repo := range []string{"o/r.git", "o/secret.git", "pub/lic.git"} {
		must(plain, seed, "init", "--quiet", "--bare", "--initial-branch=main", filepath.Join(root, repo))
		must(plain, seed, "push", "--quiet", filepath.Join(root, repo), "HEAD:refs/heads/main")
	}
	for _, repo := range []string{"o/r.git", "o/secret.git"} {
		must(plain, filepath.Join(root, repo), "config", "http.receivepack", "true")
	}
	// pub/super has pub/lic as a submodule.
	lic := strings.TrimSpace(must(plain, seed, "rev-parse", "HEAD"))
	writeFile(t, filepath.Join(seed, ".gitmodules"), "[submodule \"lic\"]\n\tpath = lic\n\turl = https://github.com/pub/lic\n")
	must(plain, seed, "add", ".gitmodules")
	must(plain, seed, "update-index", "--add", "--cacheinfo", "160000,"+lic+",lic")
	must(plain, seed, "-c", "user.name=U", "-c", "user.email=u@x", "commit", "--quiet", "-m", "Super")
	must(plain, seed, "init", "--quiet", "--bare", "--initial-branch=main", filepath.Join(root, "pub/super.git"))
	must(plain, seed, "push", "--quiet", filepath.Join(root, "pub/super.git"), "HEAD:refs/heads/main")

	var mu sync.Mutex
	var seen []string
	backendHandler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, Stderr: io.Discard}
	token := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:test-token"))
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, strings.Join([]string{r.Method + " " + r.URL.String(), r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Header.Get("Git-Protocol")}, " | "))
		mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/pub/") && r.Header.Get("Authorization") != token {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		backendHandler.ServeHTTP(w, r)
	}))
	defer github.Close()

	d := newTestDaemon(t, "{"+qubeJSON("ai", 8401, "o/r", "o/missing")+"}")
	d.qubes["ai"].github, d.qubes["ai"].client = github.URL, github.Client()
	// The stale header is from an older setup, where git went to serve by
	// itself; only the password file counts.
	writeFile(t, filepath.Join(ai, ".gitconfig"), "[url \"split-gh::https://github.com/\"]\n\tinsteadOf = https://github.com/\n"+
		"[protocol \"split-gh\"]\n\tallow = always\n[user]\n\tname = Agent\n\temail = agent@ai\n"+
		"[http \""+d.urls["ai"]+"/\"]\n\textraHeader = Authorization: Bearer stale\n")
	password := filepath.Join(ai, ".config/split-gh/password")
	writeFile(t, password, testPassword+"-ai\n")
	agent := func(port string) []string {
		return []string{"HOME=" + ai, "PATH=" + bin + ":" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C",
			"SPLIT_GH_PORT=" + port, "SPLIT_GH_GITHUB=" + github.URL + "/"}
	}
	env := agent(strings.TrimPrefix(d.urls["ai"], "http://127.0.0.1:"))

	for _, v := range []string{"0", "2"} {
		must(env, ai, "-c", "protocol.version="+v, "clone", "--quiet", "https://github.com/o/r", "r"+v)
		if out, _ := git(env, filepath.Join(ai, "r"+v), "log", "--format=%s"); out != "Initial\n" {
			t.Fatalf("protocol v%s clone has %q", v, out)
		}
		must(env, filepath.Join(ai, "r"+v), "-c", "protocol.version="+v, "fetch", "--quiet", "origin")
	}
	// Not in read_repos and public, as is its submodule: straight from GitHub.
	must(env, ai, "clone", "--quiet", "--recurse-submodules", "https://github.com/pub/super", "super")
	if _, err := os.Stat(filepath.Join(ai, "super/lic/README")); err != nil {
		t.Fatalf("submodule: %v", err)
	}
	clone := filepath.Join(ai, "r2")
	must(env, clone, "commit", "--quiet", "--allow-empty", "-m", "Agent's")
	for _, tc := range []struct {
		name, dir string
		args      []string
		want      []string
	}{
		{"push", clone, []string{"push", "origin", "HEAD:refs/heads/x"}, []string{"remote: This qube can't push to GitHub"}},
		{"push outside read_repos", filepath.Join(ai, "super"), []string{"push", "origin", "HEAD:refs/heads/x"},
			[]string{"split-gh: git went to GitHub directly for pub/super, without a token: it isn't in read_repos.", "never for a push"}},
		{"private repository outside read_repos", ai, []string{"clone", "https://github.com/o/secret"},
			[]string{"terminal prompts disabled", "split-gh: git went to GitHub directly for o/secret, without a token: it isn't in read_repos."}},
		{"missing repository", ai, []string{"clone", "https://github.com/o/missing"}, []string{"remote: split-gh: GitHub has no repository o/missing"}},
		{"not a repository", ai, []string{"ls-remote", "https://github.com/o"}, []string{`split-gh: "https://github.com/o" is not a GitHub repository URL`}},
		{"another transport", ai, []string{"ls-remote", "split-gh::ext::touch pwned"}, []string{`split-gh: "ext::touch pwned" is not a GitHub repository URL`}},
	} {
		out, err := git(env, tc.dir, tc.args...)
		for _, want := range tc.want {
			if err == nil || !strings.Contains(out, want) {
				t.Errorf("%s: %v, no %q in:\n%s", tc.name, err, want, out)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(ai, "pwned")); err == nil {
		t.Error("git ran ext::")
	}
	if out, _ := git(plain, filepath.Join(root, "o/r.git"), "for-each-ref", "--format=%(refname)"); out != "refs/heads/main\n" {
		t.Fatalf("GitHub's refs changed: %q", out)
	}

	// When serve doesn't say yes, git goes to GitHub: public repositories
	// work, private ones don't.
	writeFile(t, password, "a-wrong-password\n")
	if out, err := git(env, ai, "clone", "https://github.com/o/r", "wrong"); err == nil ||
		!strings.Contains(out, "without a token: split-gh serve wouldn't say: split-gh: wrong password") {
		t.Errorf("wrong password: %v\n%s", err, out)
	}
	writeFile(t, password, testPassword+"-ai\n")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := strings.TrimPrefix(l.Addr().String(), "127.0.0.1:")
	l.Close()
	must(agent(closed), ai, "clone", "--quiet", "https://github.com/pub/lic", "lic")
	if out, err := git(agent(closed), ai, "clone", "https://github.com/o/r", "down"); err == nil ||
		!strings.Contains(out, "without a token: nothing listens on port "+closed+" in this qube") {
		t.Errorf("serve not running: %v\n%s", err, out)
	}

	// Only serve has the token, and only for fetches from read_repos. The
	// password never reaches GitHub.
	mu.Lock()
	defer mu.Unlock()
	v2 := false
	for _, line := range seen {
		f := strings.Split(line, " | ")
		switch f[1] {
		case token:
			if !strings.Contains(f[0], " /o/r.git/") && !strings.Contains(f[0], " /o/missing.git/") || strings.Contains(f[0], "receive-pack") {
				t.Errorf("GitHub got: %s", line)
			}
			v2 = v2 || f[3] == "version=2"
		case "":
		default:
			t.Errorf("GitHub got: %s", line)
		}
		if !strings.HasPrefix(f[2], "git/") {
			t.Errorf("GitHub got: %s", line)
		}
	}
	if !v2 {
		t.Error("protocol v2 didn't reach GitHub")
	}

	for _, tc := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/fetches/o/r", http.StatusOK, "yes\n"},
		{"GET", "/fetches/O/R", http.StatusOK, "yes\n"},
		{"GET", "/fetches/o/secret", http.StatusOK, "no\n"},
		{"GET", "/fetches/o", http.StatusBadRequest, ""},
		{"GET", "/fetches/o/r/x", http.StatusBadRequest, ""},
		{"GET", "/fetches/github.com/o/r", http.StatusBadRequest, ""},
		{"GET", "/fetches/../r", http.StatusBadRequest, ""},
		{"GET", "/fetches/o/r?x=1", http.StatusBadRequest, ""},
		{"POST", "/fetches/o/r", http.StatusBadRequest, ""},
		{"GET", "/o/r/info/refs", http.StatusBadRequest, ""}, // git's old dumb protocol
		{"POST", "/o/r/info/refs?service=git-upload-pack", http.StatusBadRequest, ""},
		{"GET", "/o/r/git-upload-pack", http.StatusBadRequest, ""},
		{"GET", "/o/r/info/refs?service=git-receive-pack", http.StatusForbidden, ""},
		{"GET", "/o/r/info/lfs/objects", http.StatusNotFound, ""},
		{"GET", "/o/../info/refs?service=git-upload-pack", http.StatusForbidden, ""},
		{"GET", "/o/r/HEAD", http.StatusNotFound, ""},
	} {
		req, _ := http.NewRequest(tc.method, d.urls["ai"]+tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+testPassword+"-ai")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || tc.body != "" && string(body) != tc.body {
			t.Errorf("%s %s: %d %q", tc.method, tc.path, resp.StatusCode, body)
		}
	}

	d.close()
	terminal := d.out.String()
	for _, want := range []string{"ai: git fetch o/r info/refs -> ", "ai: git push to o/r -> refused",
		"ai: git: pub/lic isn't in read_repos, so the qube fetches it from GitHub itself, without a token",
		"ai: git: o/secret isn't in read_repos"} {
		if !strings.Contains(terminal, want) {
			t.Errorf("terminal has no %q:\n%s", want, terminal)
		}
	}
	if strings.Contains(terminal, "fetch pub/") {
		t.Errorf("serve fetched a repository outside read_repos:\n%s", terminal)
	}
	log, _ := os.ReadFile(filepath.Join(d.home, ".local/state/split-gh/log.jsonl"))
	if !strings.Contains(string(log), `"op":"git-upload-pack"`) || !strings.Contains(string(log), `"outcome":"refused: This qube can't push to GitHub; split-gh only fetches."`) ||
		strings.Contains(string(log), "test-token") || strings.Contains(string(log), testPassword) {
		t.Errorf("audit log:\n%s", log)
	}
}
