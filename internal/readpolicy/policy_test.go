package readpolicy

import (
	"reflect"
	"strings"
	"testing"
)

func TestPolicy(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"pr", "view", "123", "--comments"}, true}, {[]string{"run", "view", "--log-failed"}, true}, {[]string{"api", "repos/o/r/pulls/123/comments"}, true}, {[]string{"api", "-XPOST", "repos/o/r"}, false}, {[]string{"api", "--method=post", "repos/o/r"}, false}, {[]string{"api", "--input", "-", "repos/o/r"}, false}, {[]string{"api", "-H", "X-HTTP-Method-Override: POST", "repos/o/r"}, false}, {[]string{"api", "https://github.com/o/r"}, false}, {[]string{"api", "repos/other/r"}, false}, {[]string{"pr", "comment", "123"}, false}, {[]string{"auth", "token"}, false}, {[]string{"search", "issues", "foo", "--repo=o/r"}, true}, {[]string{"search", "issues", "repo:other/r", "--repo=o/r"}, false},
	} {
		_, e := Parse(tc.args, "o/r", "main", []string{"o/*"})
		if (e == nil) != tc.ok {
			t.Errorf("%v: %v", tc.args, e)
		}
	}
}
func FuzzParse(f *testing.F) {
	f.Add("pr", "view", "123")
	f.Fuzz(func(t *testing.T, a, b, c string) { Parse([]string{a, b, c}, "o/r", "main", []string{"o/*"}) })
}

func TestOwnerScopeAndFields(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		allowed []string
		ok      bool
	}{
		{[]string{"search", "issues", "bug", "--owner=o"}, []string{"o/*"}, true},
		{[]string{"search", "issues", "bug", "--owner=o"}, []string{"o/r"}, false},
		{[]string{"pr", "view", "https://github.com/other/r/pull/1"}, []string{"o/*"}, false},
		{[]string{"api", "repos/o/r", "-F", "data=@/etc/passwd"}, []string{"o/*"}, false},
	} {
		_, err := Parse(tc.args, "o/r", "main", tc.allowed)
		if (err == nil) != tc.ok {
			t.Errorf("%v: %v", tc.args, err)
		}
	}
}

func TestBranchCannotInjectFlags(t *testing.T) {
	for _, branch := range []string{"--web", "--repo=other/repo", "a\n--web"} {
		if _, err := Parse([]string{"pr", "view"}, "o/r", branch, []string{"o/*"}); err == nil {
			t.Errorf("accepted branch %q", branch)
		}
	}
	if _, err := Parse([]string{"pr", "view"}, "o/r", "feature/name", []string{"o/*"}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandSpecificShortFlags(t *testing.T) {
	cases := []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"api", "repos/o/r", "-H", "Accept: application/json"}, "--header=Accept: application/json", true},
		{[]string{"api", "repos/o/r", "-H", "X-HTTP-Method-Override: POST"}, "", false},
		{[]string{"pr", "list", "-B", "main"}, "--base=main", true},
		{[]string{"run", "list", "-w", "build"}, "--workflow=build", true},
		{[]string{"pr", "list", "-w"}, "", false},
		{[]string{"pr", "list", "--search=repo:other/repo"}, "", false},
	}
	for _, tc := range cases {
		got, err := Parse(tc.args, "o/r", "main", []string{"o/*"})
		if (err == nil) != tc.ok || tc.ok && !strings.Contains(strings.Join(got, " "), tc.want) {
			t.Errorf("%v: %v %v", tc.args, got, err)
		}
	}
}

func TestBranchAndWorkflowNames(t *testing.T) {
	for _, args := range [][]string{{"pr", "view", "feature/foo"}, {"workflow", "view", ".github/workflows/test.yml"}, {"release", "view", "release/v1"}} {
		if _, err := Parse(args, "o/r", "main", []string{"o/*"}); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := Parse([]string{"repo", "view", "other/repo"}, "o/r", "main", []string{"o/*"}); err == nil {
		t.Fatal("repo argument bypassed scope")
	}
}

func TestWritesAndSensitiveCommandsAlwaysRefused(t *testing.T) {
	for _, args := range [][]string{
		{"pr", "create"}, {"pr", "comment", "1"}, {"pr", "review", "1"}, {"pr", "edit", "1"}, {"pr", "merge", "1"},
		{"issue", "create"}, {"issue", "comment", "1"}, {"repo", "clone", "o/r"}, {"run", "download"}, {"release", "download"},
		{"auth", "token"}, {"config", "get", "x"}, {"alias", "list"}, {"extension", "list"}, {"secret", "list"}, {"variable", "list"}, {"ssh-key", "list"}, {"gpg-key", "list"}, {"codespace", "list"}, {"browse"},
		{"pr", "view", "--web"}, {"api", "repos/o/r", "-XPOST"}, {"api", "repos/o/r", "-X", "POST"}, {"api", "repos/o/r", "--method=post"}, {"api", "repos/o/r", "--input", "-"}, {"api", "repos/o/r", "-H", "X-HTTP-Method-Override: POST"},
	} {
		if _, err := Parse(args, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestGlobalRepositoryAndAPIPlaceholders(t *testing.T) {
	for _, args := range [][]string{{"-R", "o/r", "pr", "view", "123"}, {"--repo=o/r", "pr", "view", "123"}, {"-Ro/r", "pr", "view", "123"}, {"-R=o/r", "pr", "view", "123"}} {
		got, err := Parse(args, "o/r", "main", []string{"o/*"})
		if err != nil || !strings.Contains(strings.Join(got, " "), "--repo=o/r") {
			t.Errorf("%v: %v %v", args, got, err)
		}
	}
	for _, args := range [][]string{{"-R", "other/repo", "pr", "view", "123"}, {"--web", "pr", "view"}, {"-R", "o/r", "pr", "comment", "123"}} {
		if _, err := Parse(args, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	got, err := Parse([]string{"api", "repos/{owner}/{repo}/pulls/123/comments"}, "o/r", "main", []string{"o/*"})
	if err != nil || got[1] != "repos/o/r/pulls/123/comments" {
		t.Fatalf("placeholder expansion %v %v", got, err)
	}
}

func TestAllowedPatterns(t *testing.T) {
	for _, tc := range []struct {
		repo     string
		patterns []string
		ok       bool
	}{
		{"Org/Project", []string{"org/*"}, true}, {"other/project", []string{"org/*"}, false},
		{"org/project/extra", []string{"*/*"}, false}, {"org", []string{"*/*"}, false},
		{"org/project", []string{"org/project"}, true}, {"org/project", nil, false},
	} {
		if Allowed(tc.repo, tc.patterns) != tc.ok {
			t.Errorf("repo %q patterns %v", tc.repo, tc.patterns)
		}
	}
}

func TestSearchOutsideCheckout(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		allowed []string
		ok      bool
	}{
		{[]string{"search", "issues", "bug", "--owner=o"}, []string{"o/*"}, true},
		{[]string{"search", "code", "example", "--repo=o/r"}, []string{"o/r"}, true},
		{[]string{"search", "issues", "bug", "--owner=other"}, []string{"o/*"}, false},
		{[]string{"search", "issues", "bug"}, []string{"o/*"}, false},
		{[]string{"search", "issues", "repo:other/r", "--owner=o"}, []string{"o/*"}, false},
	} {
		_, err := Parse(tc.args, "", "", tc.allowed)
		if (err == nil) != tc.ok {
			t.Errorf("%v: %v", tc.args, err)
		}
	}
}
func TestErrorsListAllowedOptions(t *testing.T) {
	_, err := Parse([]string{"pr", "view", "--web"}, "o/r", "", []string{"o/*"})
	if err == nil || !strings.Contains(err.Error(), "--comments") || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("missing flags: %v", err)
	}
	_, err = Parse([]string{"pr", "create"}, "o/r", "", []string{"o/*"})
	if err == nil || !strings.Contains(err.Error(), "pr view") || !strings.Contains(err.Error(), "api") {
		t.Fatalf("missing commands: %v", err)
	}
}

func TestOwnerSearchRejectsCommaScope(t *testing.T) {
	if _, err := Parse([]string{"search", "issues", "bug", "--owner=o,other"}, "", "", []string{"o,other/*"}); err == nil {
		t.Fatal("comma-separated owners bypassed the allowlist")
	}
}

func TestRESTOutsideCheckout(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"repos/o/r/pulls/1/comments", true}, {"repos/other/r/pulls/1/comments", false},
		{"meta", true}, {"zen", true}, {"rate_limit", true}, {"user", false}, {"user/emails", false},
		{"repos/{owner}/{repo}", false},
	} {
		_, err := Parse([]string{"api", tc.path}, "", "", []string{"o/*"})
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.path, err)
		}
	}
}

func TestSearchFiltersCannotAddScopes(t *testing.T) {
	for _, args := range [][]string{
		{"search", "issues", "bug", "--repo=o/r", "--author=alice repo:other/r"},
		{"search", "prs", "bug", "--repo=o/r", "--label=foo ORG:other"},
		{"search", "commits", "bug", "--repo=o/r", "--committer=x user:other"},
		{"pr", "list", "--author=alice repo:other/r"},
		{"issue", "list", "--label=bug org:other"},
	} {
		if _, err := Parse(args, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted injected scope: %v", args)
		}
	}
	if _, err := Parse([]string{"search", "issues", "bug", "--repo=o/r", "--jq=env.GH_TOKEN"}, "o/r", "main", []string{"o/*"}); err == nil {
		t.Fatal("accepted token-exposing jq")
	}
}

func TestAPIFieldPlaceholders(t *testing.T) {
	args, err := Parse([]string{"api", "repos/{owner}/{repo}/pulls", "-F", "owner={owner}", "-F", "repo={repo}", "-F", "ref={branch}"}, "123/r", "topic/example", []string{"123/*"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{"repos/123/r/pulls", "--raw-field=owner=123", "--raw-field=repo=r", "--raw-field=ref=topic/example"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}

	if _, err = Parse([]string{"api", "repos/o/r", "-F", "ref={branch}"}, "o/r", "", []string{"o/*"}); err == nil {
		t.Fatal("accepted missing branch")
	}
}

func FuzzRESTReadOnly(f *testing.F) {
	for _, seed := range [][2]string{{"-X", "POST"}, {"-XPOST", ""}, {"--method=post", ""}, {"-H", "X-HTTP-Method-Override: POST"}, {"--input", "-"}, {"-H", "Accept: application/json"}, {"-f", "x=1"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		argv := []string{"api", "repos/o/r"}
		if a != "" {
			argv = append(argv, a)
		}
		if b != "" {
			argv = append(argv, b)
		}
		got, err := Parse(argv, "o/r", "main", []string{"o/*"})
		if err != nil {
			return
		}
		if len(got) < 3 || got[0] != "api" || got[1] != "repos/o/r" || got[2] != "--method=GET" {
			t.Fatalf("unexpected canonical command: %q", got)
		}
		for _, arg := range got[3:] {
			if strings.HasPrefix(arg, "--method=") || strings.HasPrefix(arg, "--input") {
				t.Fatalf("dangerous option: %q", got)
			}
			if strings.HasPrefix(arg, "--header=") && !strings.HasPrefix(strings.ToLower(strings.TrimPrefix(arg, "--header=")), "accept:") {
				t.Fatalf("dangerous header: %q", got)
			}
		}
	})
}

func TestJQAlwaysRefused(t *testing.T) {
	for _, args := range [][]string{{"api", "repos/o/r", "--jq", "env.GH_TOKEN"}, {"api", "repos/o/r", "-q", "$ENV.GH_TOKEN"}, {"pr", "view", "1", "--json=number", "--jq=env.GH_TOKEN"}, {"issue", "list", "--json=number", "-q", "env.GH_TOKEN"}} {
		if _, err := Parse(args, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted token-exposing jq: %v", args)
		}
	}
}

func TestExplicitGitHubHost(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{{"github.com/o/r", true}, {"GITHUB.COM/o/r", true}, {"other.example/o/r", false}, {"github.com/other/r", false}} {
		got, err := Parse([]string{"pr", "view", "1", "-R", tc.value}, "o/r", "main", []string{"o/*"})
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.value, err)
		}
		if tc.ok && !strings.Contains(strings.Join(got, " "), "--repo=o/r") {
			t.Errorf("not canonical: %v", got)
		}
	}
}

func TestDotPathComponentsNeverMatch(t *testing.T) {
	for _, repo := range []string{"../r", "./r", "o/..", "o/."} {
		if Allowed(repo, []string{"*/*"}) {
			t.Errorf("invalid repository allowed: %q", repo)
		}
	}
}

func TestSearchBooleanCannotBroadenScope(t *testing.T) {
	for _, args := range [][]string{
		{"search", "issues", "foo OR bar", "--repo=o/r"},
		{"search", "prs", "(foo OR bar)", "--repo=o/r"},
		{"search", "code", "foo", "OR", "bar", "--repo=o/r"},
		{"search", "issues", "foo", "--repo=o/r", "--author=alice OR bob"},
		{"pr", "list", "--search=foo OR bar"},
		{"issue", "list", "--search=(foo OR bar)"},
	} {
		if _, err := Parse(args, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted boolean scope: %v", args)
		}
	}
	if _, err := Parse([]string{"search", "issues", "bug", "--repo=or/r"}, "or/r", "main", []string{"or/*"}); err != nil {
		t.Errorf("valid repository rejected: %v", err)
	}
}

func TestAgentGraphQLAlwaysRefused(t *testing.T) {
	for _, path := range []string{"graphql", "/graphql", "GRAPHQL", "repos/o/r/../../graphql", "repos/o/r/%2e%2e/%2e%2e/graphql", "https://api.github.com/graphql"} {
		for _, grant := range [][]string{{"o/*"}, {"*/*"}} {
			args := []string{"api", path, "-f", "query={viewer{login}}"}
			if _, err := Parse(args, "o/r", "main", grant); err == nil {
				t.Errorf("accepted %s with %v", path, grant)
			}
		}
	}
}

func TestOwnerSearchWithWildcardOwnerGrants(t *testing.T) {
	for _, tc := range []struct {
		owner  string
		grants []string
		ok     bool
	}{
		{"foo", []string{"*/*"}, true}, {"team-a", []string{"team-?/*"}, true}, {"TEAM-X", []string{"team-*/*"}, true},
		{"other", []string{"team-*/*"}, false}, {"foo", []string{"foo/repo"}, false}, {"foo", []string{"foo/[a-z]*"}, false},
		{"foo,other", []string{"*/*"}, false}, {"..", []string{"*/*"}, false}, {"foo", []string{"[/*"}, false},
	} {
		_, err := Parse([]string{"search", "issues", "bug", "--owner=" + tc.owner}, "", "", tc.grants)
		if (err == nil) != tc.ok {
			t.Errorf("owner=%q grants=%v: %v", tc.owner, tc.grants, err)
		}
	}
}

func TestRESTQueryStrings(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		ok       bool
	}{
		{"repos/o/r/issues?state=closed&per_page=50", true},
		{"repos/o/r/issues?labels=bug%20fix", true},
		{"repos/o/r/issues?_method=POST", false},
		{"repos/o/r/issues?%5fmethod=POST", false},
		{"repos/o/r/issues?X-HTTP-Method-Override=POST", false},
		{"repos/o/r/issues?state=%zz", false},
		{"repos/other/r/issues?state=open", false},
		{"repos/o/r/../../graphql?query=x", false},
		{"graphql?query=x", false}, {"/graphql?query=x", false},
		{"https://github.com/o/r?x=y", false},
	} {
		got, err := Parse([]string{"api", tc.endpoint}, "o/r", "main", []string{"o/*"})
		if (err == nil) != tc.ok {
			t.Errorf("%q: %v", tc.endpoint, err)
		}
		if tc.ok && (got[1] != tc.endpoint || got[2] != "--method=GET") {
			t.Errorf("unexpected args %v", got)
		}
	}
	for _, field := range []string{"_method=POST", "X-HTTP-Method-Override=POST", "method[override]=POST", "%5fmethod=POST"} {
		if _, err := Parse([]string{"api", "repos/o/r", "-f", field}, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted %q", field)
		}
	}
}

func TestRESTQueryRejectsControlInParameterName(t *testing.T) {
	for _, endpoint := range []string{"repos/o/r/issues?%00_method=POST", "repos/o/r/issues?x%0a=1"} {
		if _, err := Parse([]string{"api", endpoint}, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted %q", endpoint)
		}
	}
}

// Exercise every phase 1 command family and its canonical scope. These are
// representative reads, rather than a restatement of the internal flag table.
func TestPhaseOneReadCommands(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		contains []string
	}{
		{[]string{"pr", "view"}, []string{"pr", "view", "feature/work", "--repo=o/r"}},
		{[]string{"pr", "view", "123", "--comments"}, []string{"--comments", "--repo=o/r"}},
		{[]string{"pr", "list", "--state", "closed"}, []string{"--state=closed", "--repo=o/r"}},
		{[]string{"pr", "diff", "123", "--name-only"}, []string{"--name-only", "--repo=o/r"}},
		{[]string{"pr", "checks", "123"}, []string{"--repo=o/r"}},
		{[]string{"issue", "view", "123", "--comments"}, []string{"--comments", "--repo=o/r"}},
		{[]string{"issue", "list", "--state", "open"}, []string{"--state=open", "--repo=o/r"}},
		{[]string{"repo", "view"}, []string{"o/r"}},
		{[]string{"label", "list"}, []string{"--repo=o/r"}},
		{[]string{"release", "list"}, []string{"--repo=o/r"}},
		{[]string{"release", "view", "v1"}, []string{"--repo=o/r"}},
		{[]string{"run", "list", "--branch", "main"}, []string{"--branch=main", "--repo=o/r"}},
		{[]string{"run", "view", "42", "--log"}, []string{"--log", "--repo=o/r"}},
		{[]string{"run", "view", "42", "--log-failed"}, []string{"--log-failed", "--repo=o/r"}},
		{[]string{"workflow", "list"}, []string{"--repo=o/r"}},
		{[]string{"workflow", "view", "ci.yml"}, []string{"--repo=o/r"}},
		{[]string{"search", "issues", "bug", "--repo=o/r"}, []string{"--repo=o/r"}},
		{[]string{"search", "prs", "fix", "--repo=o/r"}, []string{"--repo=o/r"}},
		{[]string{"search", "commits", "fix", "--repo=o/r"}, []string{"--repo=o/r"}},
		{[]string{"search", "code", "symbol", "--repo=o/r"}, []string{"--repo=o/r"}},
		{[]string{"api", "repos/o/r/pulls/123/comments"}, []string{"--method=GET"}},
	} {
		got, err := Parse(tc.args, "o/r", "feature/work", []string{"o/*"})
		if err != nil {
			t.Errorf("%v: %v", tc.args, err)
			continue
		}
		for _, want := range tc.contains {
			found := false
			for _, arg := range got {
				if arg == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%v: missing %q in %v", tc.args, want, got)
			}
		}
	}
}

func TestRepoViewUsesOnlyCheckedRepository(t *testing.T) {
	for _, args := range [][]string{{"repo", "view", "secret"}, {"repo", "view", "other/repo"}, {"repo", "view", "github.com/o/r"}} {
		if _, err := Parse(args, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted positional %v", args)
		}
	}
	got, err := Parse([]string{"repo", "view", "--repo=o/r", "--json=isPrivate"}, "o/r", "main", []string{"o/*"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"repo", "view", "o/r", "--json=isPrivate"}) {
		t.Fatalf("unexpected canonical command: %v", got)
	}
	for _, arg := range []string{"--json=name", "--template={{.}}", "--jq=.name"} {
		if _, err := Parse([]string{"workflow", "view", "ci.yml", arg}, "o/r", "main", []string{"o/*"}); err == nil {
			t.Errorf("accepted workflow view flag %s", arg)
		}
	}
}

func TestRepositoryAPIReadRoutes(t *testing.T) {
	for _, path := range []string{"repos/o/r/pulls/1/comments", "repos/o/r/compare/main...feature", "repos/o/r/actions/runs/1/jobs", "repos/o/r/git/refs/heads/main"} {
		if _, err := Parse([]string{"api", path}, "o/r", "main", []string{"o/r"}); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	for _, path := range []string{"repos/o/r/hooks", "repos/o/r/keys", "repos/o/r/invitations", "repos/o/r/actions/secrets", "repos/o/r/actions/variables", "repos/o/r/compare/main...other:branch", "repos/o/r/pulls/../hooks"} {
		if _, err := Parse([]string{"api", path}, "o/r", "main", []string{"o/r"}); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
}
