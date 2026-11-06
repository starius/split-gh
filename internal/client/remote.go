package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"split-gh/internal/readpolicy"
	"split-gh/internal/wire"
)

// githubRepoURL is what git gives git-remote-split-gh: insteadOf turns a
// GitHub URL into split-gh::https://github.com/OWNER/REPO, and git passes on
// what follows "split-gh::".
var githubRepoURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// RemoteHelper is git-remote-split-gh. In the agent qube, git's config has
// url.split-gh::https://github.com/.insteadOf https://github.com/, so git
// runs it for every GitHub repository, as git-remote-split-gh REMOTE URL.
// It asks split-gh serve whether serve fetches the repository. If serve
// says yes, git fetches through serve, which adds gh-auth's read token.
// Otherwise git fetches straight from GitHub, without a token: that works
// for public repositories, also while serve isn't running. Either way, git's
// own HTTP helper does the fetch, with git's stdin and stdout.
func RemoteHelper(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "git-remote-split-gh is for git, which runs it as git-remote-split-gh REMOTE URL")
		return 2
	}
	remote, url := args[0], args[1]
	m := githubRepoURL.FindStringSubmatch(url)
	if m == nil {
		fmt.Fprintf(os.Stderr, "split-gh: %q is not a GitHub repository URL, https://github.com/OWNER/REPO\n", url)
		return 1
	}
	repo := m[1]
	if _, ok := readpolicy.CanonicalRepo(repo); !ok {
		fmt.Fprintf(os.Stderr, "split-gh: %q is not a GitHub repository URL, https://github.com/OWNER/REPO\n", url)
		return 1
	}
	path := strings.TrimPrefix(url, "https://github.com/")
	password, why := askServe(repo)
	var cmd *exec.Cmd
	if password != "" {
		base := "http://127.0.0.1:" + port() + "/"
		// Only requests to serve carry the password. The empty value clears
		// any header for serve that the config has.
		header := "http." + base + ".extraHeader"
		cmd = exec.Command("git", "-c", header+"=", "-c", header+"=Authorization: Bearer "+password,
			"remote-http", remote, base+path)
	} else {
		cmd = exec.Command("git", "remote-https", remote, github()+path)
		// Nothing in this qube can log in to GitHub, so git doesn't ask.
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if password == "" {
		fmt.Fprintf(os.Stderr, "split-gh: git went to GitHub directly for %s, without a token: %s.\n"+
			"That works only for public repositories, and never for a push. If %s is private,\n"+
			"ask the user to run split-gh serve in gh-auth, with it in read_repos.\n"+
			"To hand over commits, use split-gh send.\n", repo, why, repo)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return exit.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "split-gh:", err)
	return 1
}

// askServe asks split-gh serve whether it fetches repo for this qube. If it
// does, it returns the password, for git's requests to serve. Otherwise it
// says why git goes to GitHub itself.
func askServe(repo string) (password, why string) {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".config/split-gh/password"))
	if err != nil {
		return "", fmt.Sprintf("split-gh isn't set up in this qube (%v)", err)
	}
	password = strings.TrimSpace(string(b))
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port()+wire.FetchesPath+repo, nil)
	if err != nil {
		return "", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+password)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "", "nothing listens on port " + port() + " in this qube, where qvm-connect-tcp forwards to split-gh serve"
	}
	if err != nil {
		return "", fmt.Sprintf("split-gh serve didn't answer (%v)", err)
	}
	defer resp.Body.Close()
	b, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	answer := strings.TrimSpace(string(b))
	switch {
	case resp.StatusCode != http.StatusOK:
		first, _, _ := strings.Cut(answer, "\n")
		return "", "split-gh serve wouldn't say: " + first
	case answer == wire.Yes:
		return password, ""
	case answer == wire.No:
		return "", "it isn't in read_repos"
	}
	return "", fmt.Sprintf("split-gh serve answered %q", answer)
}

// github is where git fetches the repositories that serve doesn't. Tests
// set SPLIT_GH_GITHUB to a local server.
func github() string {
	if g := os.Getenv("SPLIT_GH_GITHUB"); g != "" {
		return g
	}
	return "https://github.com/"
}
