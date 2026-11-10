package readpolicy

import (
	"errors"
	"strconv"
	"strings"
)

// ParseProjectView requires an explicit owner and positive project number.
// Projects belong to owners, not repositories, and use a separate allowlist.
func ParseProjectView(argv, allowed []string) ([]string, error) {
	args, err := NormalizeGlobal(argv)
	if err != nil {
		return nil, err
	}
	if len(args) < 2 || args[0] != "project" || args[1] != "view" {
		return nil, errors.New("only project view is allowed")
	}
	var owner, number string
	seen := map[string]bool{}
	var opts []string
	for i := 2; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if number != "" {
				return nil, errors.New("project view requires one project number")
			}
			n, err := strconv.Atoi(a)
			if err != nil || n <= 0 || strconv.Itoa(n) != a {
				return nil, errors.New("project number must be a positive decimal integer")
			}
			number = a
			continue
		}
		name, value, has := strings.Cut(a, "=")
		if name == "-t" {
			name = "--template"
		} else if strings.HasPrefix(name, "-t") && !strings.HasPrefix(name, "--") {
			value, name, has = strings.TrimPrefix(a, "-t"), "--template", true
		}
		if name != "--owner" && name != "--format" && name != "--template" {
			return nil, errors.New("project view allows only --owner, --format, and --template; --jq runs in the client")
		}
		if seen[name] {
			return nil, errors.New("duplicate project view flag")
		}
		seen[name] = true
		if !has {
			i++
			if i >= len(args) {
				return nil, errors.New("missing project view flag value")
			}
			value = args[i]
		}
		if name == "--owner" {
			owner = value
		} else {
			if name == "--format" && value != "json" {
				return nil, errors.New("project view format must be json")
			}
			opts = append(opts, name+"="+value)
		}
	}
	if number == "" || !Allowed(owner+"/"+number, allowed) {
		return nil, errors.New("project not allowed; specify a number and --owner, and ask the user to configure read_projects")
	}
	return append([]string{"project", "view", number, "--owner=" + owner}, opts...), nil
}
