package readpolicy

import (
	"reflect"
	"testing"
)

func TestExtractClientJQ(t *testing.T) {
	for _, tc := range []struct {
		in, want []string
		expr     string
		found    bool
	}{
		{[]string{"api", "repos/o/r", "-q", ".body"}, []string{"api", "repos/o/r"}, ".body", true},
		{[]string{"-R", "o/r", "pr", "view", "1", "--json=body", "--jq=.body"}, []string{"pr", "view", "--repo=o/r", "1", "--json=body"}, ".body", true},
		{[]string{"pr", "list", "--search", "--jq=literal"}, []string{"pr", "list", "--search", "--jq=literal"}, "", false},
		{[]string{"api", "repos/o/r", "-q.body"}, []string{"api", "repos/o/r"}, ".body", true},
		{[]string{"pr", "view", "1", "--unknown", "--jq=.body"}, []string{"pr", "view", "1", "--unknown", "--jq=.body"}, "", false},
	} {
		got, expr, found, err := ExtractClientJQ(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) || expr != tc.expr || found != tc.found {
			t.Errorf("%v => %v %q %v %v", tc.in, got, expr, found, err)
		}
	}
	if _, _, _, err := ExtractClientJQ([]string{"api", "meta", "-q"}); err == nil {
		t.Fatal("accepted missing jq expression")
	}
}

func TestClientRepoFlagSkipsOtherValues(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		repo  string
		found bool
	}{
		{[]string{"pr", "view", "1", "-R", "o/r"}, "o/r", true},
		{[]string{"-R=github.com/o/r", "pr", "view", "1"}, "github.com/o/r", true},
		{[]string{"pr", "list", "--search", "--repo=not-an-option"}, "", false},
		{[]string{"pr", "view", "--template", "--repo=not-an-option"}, "", false},
		{[]string{"api", "repos/o/r", "-f", "--repo=not-an-option"}, "", false},
		{[]string{"pr", "threads", "1", "-Ro/r"}, "o/r", true},
		{[]string{"pr", "view", "--unknown", "--repo=o/r"}, "", false},
	} {
		repo, found := ClientRepoFlag(tc.args)
		if repo != tc.repo || found != tc.found {
			t.Errorf("%v => %q, %v", tc.args, repo, found)
		}
	}
}
