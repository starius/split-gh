package readpolicy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseProjectRead requires an explicit owner and, except for list, a number.
// Projects belong to owners, not repositories, and use a separate allowlist.
func ParseProjectRead(argv, allowed []string) ([]string, error) {
	args, err := NormalizeGlobal(argv)
	if err != nil {
		return nil, err
	}
	if len(args) < 2 || args[0] != "project" {
		return nil, errors.New("project read command required")
	}
	command := args[1]
	if command == "ls" {
		command = "list"
	}
	defs, ok := commands["project "+command]
	if !ok {
		return nil, errors.New("only project view, item-list, field-list, and list are allowed")
	}
	flags := map[string]bool{}
	for _, name := range strings.Split(defs, ",") {
		flags["--"+name] = true
	}
	delete(flags, "--jq") // Never evaluate jq beside the token.
	var owner, number string
	seen := map[string]bool{}
	var opts []string
	for i := 2; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if number != "" || command == "list" {
				return nil, errors.New("unexpected project argument")
			}
			n, err := strconv.Atoi(a)
			if err != nil || n <= 0 || strconv.Itoa(n) != a {
				return nil, errors.New("project number must be a positive decimal integer")
			}
			number = a
			continue
		}
		name, value, has := strings.Cut(a, "=")
		if !strings.HasPrefix(name, "--") && len(name) >= 2 && (name[:2] == "-t" || name[:2] == "-L") {
			short := name[:2]
			if len(name) > 2 {
				value, has = name[2:], true
			}
			name = "--template"
			if short == "-L" {
				name = "--limit"
			}
		}
		if !flags[name] {
			return nil, fmt.Errorf("flag %q is not allowed for project %s; --jq runs in the client", name, command)
		}
		if seen[name] && name != "--field" && name != "--field-id" {
			return nil, errors.New("duplicate project flag")
		}
		seen[name] = true
		if name == "--closed" {
			if !has {
				value = "true"
			}
			b, err := strconv.ParseBool(value)
			if err != nil {
				return nil, errors.New("invalid --closed value")
			}
			opts = append(opts, "--closed="+strconv.FormatBool(b))
			continue
		}
		if !has {
			i++
			if i >= len(args) {
				return nil, errors.New("missing project flag value")
			}
			value = args[i]
		}
		if name == "--owner" {
			owner = value
		} else {
			if name == "--limit" {
				n, err := strconv.Atoi(value)
				if err != nil || n <= 0 {
					return nil, errors.New("project limit must be a positive integer")
				}
				value = strconv.Itoa(n)
			}
			if name == "--format" && value != "json" {
				return nil, errors.New("project format must be json")
			}
			opts = append(opts, name+"="+value)
		}
	}
	if command == "list" {
		if !allowedOwner(owner, allowed) {
			return nil, errors.New("project list requires --owner and an owner-wide read_projects grant such as OWNER/*")
		}
		return append([]string{"project", "list", "--owner=" + owner}, opts...), nil
	}
	if number == "" || !Allowed(owner+"/"+number, allowed) {
		return nil, errors.New("project not allowed; specify a number and --owner, and ask the user to configure read_projects")
	}
	return append([]string{"project", command, number, "--owner=" + owner}, opts...), nil
}
