package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"split-gh/internal/wire"
	"strings"
	"testing"
)

func TestShimSendsRequest(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "request.json")
	fakeGit := "#!/bin/sh\ncase \"$*\" in\n'remote get-url gh-resolved') echo https://github.com/owner/project.git;;\n'branch --show-current') echo feature/foo;;\nesac\n"
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
