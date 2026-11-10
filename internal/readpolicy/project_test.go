package readpolicy

import (
	"reflect"
	"testing"
)

func TestProjectViewPolicy(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		allowed []string
		want    bool
	}{
		{[]string{"12", "--owner", "example"}, []string{"example/12"}, true},
		{[]string{"12", "--owner=EXAMPLE", "--format=json", "-t", "{{.title}}"}, []string{"example/*"}, true},
		{[]string{"12", "--owner=other"}, []string{"example/12"}, false},
		{[]string{"13", "--owner=example"}, []string{"example/12"}, false},
		{[]string{"12", "--owner=example"}, nil, false},
		{[]string{"12"}, []string{"*/*"}, false},
		{[]string{"--owner=example"}, []string{"*/*"}, false},
		{[]string{"12", "--owner=@me"}, []string{"*/*"}, false},
		{[]string{"12", "--owner=../example"}, []string{"*/*"}, false},
		{[]string{"0", "--owner=example"}, []string{"*/*"}, false},
		{[]string{"01", "--owner=example"}, []string{"*/*"}, false},
		{[]string{"12", "13", "--owner=example"}, []string{"*/*"}, false},
		{[]string{"12", "--owner=example", "--owner=other"}, []string{"*/*"}, false},
		{[]string{"12", "--owner=example", "--web"}, []string{"*/*"}, false},
		{[]string{"12", "--owner=example", "--repo=example/project"}, []string{"*/*"}, false},
		{[]string{"12", "--owner=example", "--jq=env.GH_TOKEN"}, []string{"*/*"}, false},
		{[]string{"12", "--owner=example", "--format=yaml"}, []string{"*/*"}, false},
		{[]string{"12", "--owner"}, []string{"*/*"}, false},
	} {
		args := append([]string{"project", "view"}, tc.args...)
		_, err := ParseProjectView(args, tc.allowed)
		if (err == nil) != tc.want {
			t.Errorf("%v with %v: %v, want allowed=%v", args, tc.allowed, err, tc.want)
		}
	}
	if _, err := Parse([]string{"project", "view", "12", "--owner=example"}, "example/project", "main", []string{"example/*"}); err == nil {
		t.Fatal("repository allowlist granted project access")
	}
}

func TestProjectViewClientJQ(t *testing.T) {
	args, expression, found, err := ExtractClientJQ([]string{"project", "view", "12", "--owner", "example", "--format=json", "--jq", ".title"})
	if err != nil || !found || expression != ".title" {
		t.Fatalf("extract jq: %v %q %v %v", args, expression, found, err)
	}
	got, err := ParseProjectView(args, []string{"example/12"})
	want := []string{"project", "view", "12", "--owner=example", "--format=json"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
}
