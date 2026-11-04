package readpolicy

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// CanonicalRepo accepts gh's explicit github.com/OWNER/REPO form, but no
// alternate hosts; the service only has policy and credentials for GitHub.com.
func CanonicalRepo(repo string) (string, bool) {
	if len(repo) >= 11 && strings.EqualFold(repo[:11], "github.com/") {
		repo = repo[11:]
	}
	if !repoRE.MatchString(repo) {
		return repo, false
	}
	parts := strings.Split(repo, "/")
	if parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return repo, false
	}
	return repo, true
}

func Allowed(repo string, patterns []string) bool {
	canonical, ok := CanonicalRepo(repo)
	if !ok || canonical != repo {
		return false
	}
	for _, p := range patterns {
		if ok, _ := path.Match(strings.ToLower(p), strings.ToLower(repo)); ok {
			return true
		}
	}
	return false
}

var commands = map[string]string{
	"pr view": "comments,json,jq,template", "pr list": "state,author,assignee,base,head,label,limit,search,draft,json,jq,template", "pr diff": "name-only,patch,color", "pr checks": "json,jq,template,required,watch,interval,fail-fast", "issue view": "comments,json,jq,template", "issue list": "state,author,assignee,label,limit,search,milestone,mention,json,jq,template", "repo view": "json,jq,template", "label list": "limit,search,json,jq,template", "release list": "limit,exclude-drafts,exclude-pre-releases,json,jq,template", "release view": "json,jq,template", "run list": "branch,commit,event,limit,status,user,workflow,json,jq,template", "run view": "log,log-failed,job,attempt,exit-status,json,jq,template,verbose", "workflow list": "all,limit,json,jq,template", "workflow view": "ref,yaml,json,jq,template", "search issues": "repo,owner,limit,sort,order,state,author,assignee,label,json,jq,template", "search prs": "repo,owner,limit,sort,order,state,author,assignee,label,json,jq,template", "search commits": "repo,owner,limit,sort,order,author,committer,json,jq,template", "search code": "repo,owner,limit,filename,extension,language,json,jq,template",
}
var bools = map[string]bool{"comments": true, "name-only": true, "patch": true, "draft": true, "required": true, "watch": true, "fail-fast": true, "log": true, "log-failed": true, "exit-status": true, "verbose": true, "all": true, "yaml": true, "exclude-drafts": true, "exclude-pre-releases": true}

func containsForbiddenScope(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "repo:") || strings.Contains(lower, "org:") || strings.Contains(lower, "user:") {
		return true
	}
	// A boolean disjunction or grouping can change how GitHub binds the
	// repository qualifier added by gh. Reject it unless we can prove scope.
	if strings.ContainsAny(value, "()") {
		return true
	}
	for _, token := range strings.FieldsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`"'{}[]`, r) }) {
		if strings.EqualFold(token, "OR") || strings.EqualFold(token, "NOT") {
			return true
		}
	}
	return false
}

// Short options differ between gh subcommands; never infer a shared meaning.
func shortFlag(command, name string) string {
	common := map[string]string{"R": "repo", "L": "limit", "q": "jq", "t": "template"}
	per := map[string]map[string]string{
		"api":     {"X": "method", "H": "header", "f": "raw-field", "F": "field"},
		"pr view": {"c": "comments"}, "issue view": {"c": "comments"},
		"pr list":    {"a": "assignee", "A": "author", "B": "base", "H": "head", "d": "draft", "l": "label", "s": "state", "S": "search"},
		"issue list": {"a": "assignee", "A": "author", "l": "label", "s": "state", "S": "search", "m": "milestone"},
		"run list":   {"b": "branch", "c": "commit", "e": "event", "s": "status", "u": "user", "w": "workflow"},
		"pr checks":  {"i": "interval"},
		"run view":   {"j": "job", "a": "attempt", "v": "verbose"},
	}
	if v := per[command][name]; v != "" {
		return v
	}
	return common[name]
}

// NormalizeGlobal moves the inherited repository flag after the subcommand so
// the same strict validation applies regardless of where the caller put it.
func NormalizeGlobal(argv []string) ([]string, error) {
	var opts []string
	for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
		a := argv[0]
		argv = argv[1:]
		switch {
		case a == "-R" || a == "--repo":
			if len(argv) == 0 {
				return nil, errors.New("missing repository value")
			}
			opts = append(opts, "--repo="+argv[0])
			argv = argv[1:]
		case strings.HasPrefix(a, "-R") && len(a) > 2:
			opts = append(opts, "--repo="+strings.TrimPrefix(a[2:], "="))
		case strings.HasPrefix(a, "--repo="):
			opts = append(opts, a)
		default:
			return nil, fmt.Errorf("global flag %q not allowed", a)
		}
	}
	if len(opts) == 0 {
		return argv, nil
	}
	if len(argv) < 2 || argv[0] == "api" {
		return nil, errors.New("repository flag requires a supported command")
	}
	normalized := append([]string{}, argv[:2]...)
	normalized = append(normalized, opts...)
	return append(normalized, argv[2:]...), nil
}

func Parse(argv []string, repo, branch string, allowed []string) ([]string, error) {
	var err error
	argv, err = NormalizeGlobal(argv)
	if err != nil {
		return nil, err
	}
	if len(argv) < 1 {
		return nil, errors.New("missing command")
	}
	api := argv[0] == "api"
	key := "api"
	start := 1
	if !api {
		if len(argv) < 2 {
			return nil, errors.New("missing command")
		}
		key = argv[0] + " " + argv[1]
		start = 2
		if _, ok := commands[key]; !ok {
			names := make([]string, 0, len(commands)+2)
			for name := range commands {
				names = append(names, name)
			}
			names = append(names, "api", "pr threads")
			sort.Strings(names)
			return nil, fmt.Errorf("command %q is not allowed; writes to GitHub are done by the user; allowed: %s", key, strings.Join(names, ", "))
		}
	}
	if !api && !strings.HasPrefix(key, "search ") && !Allowed(repo, allowed) {
		return nil, errors.New("repository not allowed")
	}
	flags := map[string]bool{}
	if api {
		for _, s := range []string{"method", "header", "field", "raw-field", "paginate", "slurp", "jq", "template"} {
			flags[s] = true
		}
	} else {
		for _, s := range strings.Split(commands[key], ",") {
			flags[s] = true
		}
		flags["repo"] = true
	}
	// gh's jq evaluator exposes env and $ENV, including GH_TOKEN.
	// Filtering must happen on the caller side after the response arrives.
	delete(flags, "jq")
	var pos, opts []string
	scoped := false
	for i := start; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		if a == "--" {
			return nil, errors.New("-- is not allowed")
		}
		name, val, has := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if !strings.HasPrefix(a, "--") {
			if len(name) > 1 {
				val = name[1:]
				has = true
				name = name[:1]
			}
			if x := shortFlag(key, name); x != "" {
				name = x
			}
		}
		if !flags[name] {
			names := make([]string, 0, len(flags))
			for name := range flags {
				names = append(names, "--"+name)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("flag %q not allowed for %s; allowed: %s", name, key, strings.Join(names, ", "))
		}
		if !has && !bools[name] && name != "paginate" && name != "slurp" {
			i++
			if i >= len(argv) {
				return nil, errors.New("missing flag value")
			}
			val = argv[i]
			has = true
		}
		if !has {
			opts = append(opts, "--"+name)
			continue
		}
		if (name == "paginate" || name == "slurp") && has {
			v, err := strconv.ParseBool(val)
			if err != nil {
				return nil, fmt.Errorf("invalid --%s value", name)
			}
			val = strconv.FormatBool(v)
		}
		if api && name == "field" {
			if fieldKey, fieldValue, ok := strings.Cut(val, "="); ok && (fieldValue == "{owner}" || fieldValue == "{repo}" || fieldValue == "{branch}") {
				if !Allowed(repo, allowed) {
					return nil, errors.New("repository required for field placeholder")
				}
				parts := strings.Split(repo, "/")
				resolved := branch
				if fieldValue == "{owner}" {
					resolved = parts[0]
				} else if fieldValue == "{repo}" {
					resolved = parts[1]
				} else if branch == "" {
					return nil, errors.New("branch required for field placeholder")
				}
				// Preserve gh's string type for placeholders, including numeric names.
				name = "raw-field"
				val = fieldKey + "=" + resolved
			}
		}
		// gh combines filters into a search query; a qualifier embedded in
		// any filter could otherwise add a second, unapproved repository.
		if (strings.HasPrefix(key, "search ") || key == "pr list" || key == "issue list") && name != "jq" && name != "template" && name != "json" && name != "repo" && name != "owner" && containsForbiddenScope(val) {
			return nil, errors.New("search scope qualifiers and boolean operators are not allowed")
		}
		switch name {
		case "repo":
			canonical, ok := CanonicalRepo(val)
			if !ok {
				return nil, errors.New("repository not allowed")
			}
			val = canonical
			if !Allowed(val, allowed) {
				return nil, errors.New("repository not allowed")
			}
			if strings.HasPrefix(key, "search ") {
				scoped = true
			} else if !strings.EqualFold(val, repo) {
				return nil, errors.New("repository mismatch")
			}
		case "owner":
			permitted := false
			for _, pattern := range allowed {
				if strings.EqualFold(pattern, val+"/*") && Allowed(val+"/x", []string{pattern}) {
					permitted = true
					break
				}
			}
			if !permitted {
				return nil, errors.New("owner is not fully allowed")
			}
			scoped = true
		case "method":
			if !strings.EqualFold(val, "GET") {
				return nil, errors.New("only GET is allowed")
			}
			continue
		case "header":
			if !strings.HasPrefix(strings.ToLower(val), "accept:") || strings.ContainsAny(val, "\r\n") {
				return nil, errors.New("only Accept header is allowed")
			}
		case "field", "raw-field":
			if name == "field" && strings.Contains(val, "=@") {
				return nil, errors.New("file fields are not allowed")
			}

		}
		opts = append(opts, "--"+name+"="+val)
	}
	if api {
		if len(pos) != 1 {
			return nil, errors.New("api requires one path")
		}
		p := pos[0]
		if strings.Contains(p, "{owner}") || strings.Contains(p, "{repo}") {
			if !Allowed(repo, allowed) {
				return nil, errors.New("repository not allowed")
			}
			repoParts := strings.Split(repo, "/")
			p = strings.ReplaceAll(p, "{owner}", repoParts[0])
			p = strings.ReplaceAll(p, "{repo}", repoParts[1])
		}
		if strings.EqualFold(strings.TrimPrefix(p, "/"), "graphql") {
			return nil, errors.New("agent-written GraphQL is not allowed")
		}

		// These exact endpoints are not scoped to an account or repository.
		switch p {
		case "meta", "zen", "emojis", "rate_limit":
			return append([]string{"api", p, "--method=GET"}, opts...), nil
		}
		parts := strings.Split(p, "/")
		if len(parts) < 3 || parts[0] != "repos" || !Allowed(parts[1]+"/"+parts[2], allowed) || strings.ContainsAny(p, "?#%\\") || strings.Contains(p, "..") {
			return nil, errors.New("API path not allowed")
		}
		return append([]string{"api", p, "--method=GET"}, opts...), nil
	}
	if !strings.HasPrefix(key, "search ") {
		for _, term := range pos {
			allowSlash := key == "pr view" || key == "pr diff" || key == "pr checks" || key == "workflow view" || key == "release view"
			if strings.ContainsAny(term, "\\\r\n:") || (!allowSlash && strings.Contains(term, "/")) {
				return nil, errors.New("repository URLs and paths are not allowed as arguments")
			}
		}
	}
	if strings.HasPrefix(key, "search ") {
		for _, term := range pos {
			if containsForbiddenScope(term) {
				return nil, errors.New("search scope qualifiers and boolean operators are not allowed")
			}
		}
		if !scoped {
			return nil, errors.New("search requires --repo or --owner")
		}
	} else {
		opts = append(opts, "--repo="+repo)
	}
	if key == "pr view" && len(pos) == 0 && branch != "" {
		if strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, ":\r\n") {
			return nil, errors.New("invalid branch")
		}
		pos = append(pos, branch)
	}
	return append(append(strings.Split(key, " "), pos...), opts...), nil
}
