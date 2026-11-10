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
		_, err := ParseProjectRead(args, tc.allowed)
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
	got, err := ParseProjectRead(args, []string{"example/12"})
	want := []string{"project", "view", "12", "--owner=example", "--format=json"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
}

func TestProjectReadCommands(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		allowed []string
		want    bool
	}{
		{[]string{"item-list", "12", "--owner=example", "-L50", "--query=assignee:alice", "--field=Status", "--field=Priority", "--field-id=FIELD_1"}, []string{"example/12"}, true},
		{[]string{"field-list", "12", "--owner=example", "--limit=50"}, []string{"example/12"}, true},
		{[]string{"list", "--owner=example", "--closed", "--limit=10"}, []string{"example/*"}, true},
		{[]string{"ls", "--owner=example", "--closed=false"}, []string{"example/*"}, true},
		{[]string{"list", "--owner=example"}, []string{"example/12"}, false},
		{[]string{"list", "--owner=other"}, []string{"example/*"}, false},
		{[]string{"list", "--owner=@me"}, []string{"*/*"}, false},
		{[]string{"list", "12", "--owner=example"}, []string{"example/*"}, false},
		{[]string{"list", "--owner=example", "--closed=maybe"}, []string{"example/*"}, false},
		{[]string{"item-list", "13", "--owner=example"}, []string{"example/12"}, false},
		{[]string{"field-list", "12", "--owner=other"}, []string{"example/12"}, false},
		{[]string{"item-list", "--owner=example"}, []string{"example/*"}, false},
		{[]string{"field-list", "12", "--owner=example", "--query=bug"}, []string{"example/*"}, false},
		{[]string{"view", "12", "--owner=example", "--limit=10"}, []string{"example/*"}, false},
		{[]string{"item-list", "12", "--owner=example", "--limit=-1"}, []string{"example/*"}, false},
		{[]string{"field-list", "12", "--owner=example", "--limit=0"}, []string{"example/*"}, false},
		{[]string{"list", "--owner=example", "--web"}, []string{"example/*"}, false},
		{[]string{"item-list", "12", "--owner=example", "--jq=env.GH_TOKEN"}, []string{"example/*"}, false},
	} {
		args := append([]string{"project"}, tc.args...)
		_, err := ParseProjectRead(args, tc.allowed)
		if (err == nil) != tc.want {
			t.Errorf("%v with %v: %v, want allowed=%v", args, tc.allowed, err, tc.want)
		}
	}
	for _, command := range []string{"close", "copy", "create", "delete", "edit", "field-create", "field-delete", "item-add", "item-archive", "item-create", "item-delete", "item-edit", "link", "mark-template", "unlink"} {
		if _, err := ParseProjectRead([]string{"project", command, "12", "--owner=example"}, []string{"*/*"}); err == nil {
			t.Errorf("project write %q accepted", command)
		}
	}
}

func TestProjectReadClientJQ(t *testing.T) {
	for _, args := range [][]string{
		{"project", "item-list", "12", "--owner=example", "-L50", "--field=Status", "--field=Priority", "--format=json", "-q", ".items"},
		{"project", "field-list", "12", "--owner=example", "--format=json", "--jq=.fields"},
		{"project", "ls", "--owner=example", "--closed", "--format=json", "--jq=.projects"},
	} {
		clean, _, found, err := ExtractClientJQ(args)
		if err != nil || !found {
			t.Fatalf("extract %v: %v, found=%v", args, err, found)
		}
		if _, err := ParseProjectRead(clean, []string{"example/*"}); err != nil {
			t.Fatalf("parse %v: %v", clean, err)
		}
	}
}
