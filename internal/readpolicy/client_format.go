package readpolicy

import (
	"errors"
	"strings"
)

// ExtractClientJQ removes a single jq option without mistaking another flag's
// value for an option. This is client convenience, never server authorization.
func ExtractClientJQ(argv []string) ([]string, string, bool, error) {
	args, err := NormalizeGlobal(argv)
	if err != nil || len(args) == 0 {
		return argv, "", false, err
	}
	key := "api"
	start := 1
	if args[0] != "api" {
		if len(args) < 2 {
			return argv, "", false, nil
		}
		key = args[0] + " " + args[1]
		start = 2
	}
	flags := map[string]bool{"jq": true}
	if key == "api" {
		for _, s := range []string{"method", "header", "field", "raw-field", "paginate", "slurp", "template"} {
			flags[s] = true
		}
	} else if key == "pr threads" {
		flags["repo"] = true
	} else if defs, ok := commands[key]; ok {
		for _, s := range strings.Split(defs, ",") {
			flags[s] = true
		}
		flags["repo"] = true
	} else {
		return argv, "", false, nil
	}
	result := append([]string{}, args[:start]...)
	var expression string
	found, template := false, false
	for i := start; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			result = append(result, a)
			continue
		}
		if a == "--" {
			return argv, "", false, nil
		}
		name, value, has := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if !strings.HasPrefix(a, "--") {
			if len(name) > 1 {
				value = name[1:]
				name = name[:1]
				has = true
			}
			if v := shortFlag(key, name); v != "" {
				name = v
			}
		}
		if !flags[name] {
			return argv, "", false, nil
		}
		if name == "jq" {
			if found {
				return nil, "", false, errors.New("multiple --jq flags")
			}
			if !has {
				i++
				if i >= len(args) {
					return nil, "", false, errors.New("missing --jq value")
				}
				value = args[i]
			}
			found = true
			expression = value
			continue
		}
		if name == "template" {
			template = true
		}
		result = append(result, a)
		if !has && !bools[name] && name != "paginate" && name != "slurp" {
			i++
			if i >= len(args) {
				return argv, "", false, nil
			}
			result = append(result, args[i])
		}
	}
	if found && template {
		return nil, "", false, errors.New("--jq and --template cannot be combined")
	}
	return result, expression, found, nil
}
