package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"split-gh/internal/wire"
	"strconv"
	"strings"
	"testing"
)

func TestShimSendsRequest(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "request.json")
	fakeGit := "#!/bin/sh\ncase \"$*\" in\n'remote get-url -- origin') echo https://github.com/owner/project.git;;\n'branch --show-current') echo feature/foo;;\nesac\n"
	fakeRPC := "#!/bin/sh\n/bin/cat > \"$SPLIT_GH_TEST_REQUEST\"\nprintf '\\170\\000\\000\\000\\001\\000'\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(fakeGit), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qrexec-client-vm"), []byte(fakeRPC), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SPLIT_GH_TEST_REQUEST", out)
	t.Setenv("GH_REPO", "")
	if code := Main([]string{"pr", "view", "123", "--comments"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var q wire.Request
	if err = json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if q.Repo != "owner/project" || q.Branch != "feature/foo" || strings.Join(q.Argv, " ") != "pr view 123 --comments" || q.V != 1 {
		t.Fatalf("unexpected request: %+v", q)
	}
	if code := Main([]string{"pr", "list", "--search", "--repo=not-an-option"}); code != 0 {
		t.Fatal(code)
	}
	data, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if q.Repo != "owner/project" {
		t.Fatalf("flag value treated as repository: %+v", q)
	}
	t.Setenv("GH_REPO", "github.com/env/project")
	if code := Main([]string{"pr", "view", "1"}); code != 0 {
		t.Fatal(code)
	}
	data, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if q.Repo != "env/project" {
		t.Fatalf("GH_REPO ignored: %+v", q)
	}
	if code := Main([]string{"pr", "view", "1", "-R", "github.com/override/repo"}); code != 0 {
		t.Fatal(code)
	}
	data, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if q.Repo != "override/repo" {
		t.Fatalf("-R ignored: %+v", q)
	}
}

func TestRemoteURLs(t *testing.T) {
	for _, tc := range []struct{ url, repo string }{
		{"https://github.com/o/r.git", "o/r"}, {"git@github.com:o/r.git", "o/r"}, {"ssh://git@github.com/o/r", "o/r"},
	} {
		m := remoteRE.FindStringSubmatch(tc.url)
		if m == nil || m[1] != tc.repo {
			t.Errorf("%q: %q", tc.url, m)
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
	t.Setenv("HOME", dir)
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
	rpcdir := filepath.Join(dir, "rpc")
	if err := os.Mkdir(rpcdir, 0700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "request.json")
	script := "#!/bin/sh\n/bin/cat > " + strconv.Quote(capture) + "\nprintf '\\170\\000\\000\\000\\001\\000'\n"
	if err := os.WriteFile(filepath.Join(rpcdir, "qrexec-client-vm"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", rpcdir+string(os.PathListSeparator)+filepath.Dir(gitPath))
	t.Setenv("GH_REPO", "")
	if code := Main([]string{"pr", "view", "1", "-R", "override/repo"}); code != 0 {
		t.Fatalf("explicit override failed: %d", code)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var request wire.Request
	if err = json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if request.Repo != "override/repo" {
		t.Fatalf("wrong override: %+v", request)
	}
	run("config", "--unset", "remote.custom.remote.gh-resolved")
	run("remote", "remove", "upstream")
	if got, err := inferRepo(); err != nil || got != "fork/project" {
		t.Fatalf("origin fallback: %q %v", got, err)
	}
}
