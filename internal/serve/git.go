package serve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"split-gh/internal/readpolicy"
)

const (
	// gitDeadline bounds one request of a fetch, such as a clone's pack.
	gitDeadline = 30 * time.Minute
	// gitIdle ends a request when no data moved for this long.
	gitIdle = time.Minute
	// maxGitRequest bounds what the agent sends: the refs it wants and has.
	maxGitRequest = 10 << 20
)

// pushRefusal is what git shows the agent, as "remote:" lines, for a push.
const pushRefusal = `This qube can't push to GitHub; split-gh only fetches.
Hand over commits with split-gh send. The user reviews, signs and pushes them.`

// gitPath matches the requests of git's smart HTTP protocol: the two of a
// fetch, and a push's, which is refused.
var gitPath = regexp.MustCompile(`^/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

// serveGit passes a git fetch from a repository in the qube's read_repos
// on to GitHub, with the read token. In the agent qube, git has
// url.http://127.0.0.1:8401/.insteadOf https://github.com/ in its config.
func (q *qube) serveGit(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	what := r.Method + " " + r.URL.Path
	outcome := ""
	defer func() { q.printf("git %s -> %s (%.1fs)", what, outcome, time.Since(started).Seconds()) }()
	refuse := func(status int, msg string) {
		outcome = "refused: " + strings.TrimPrefix(strings.SplitN(msg, "\n", 2)[0], "split-gh: ")
		fail(w, msg, status)
	}
	m := gitPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		refuse(http.StatusNotFound, "split-gh: not found; it answers only gh and git fetches from GitHub")
		return
	}
	repo, op := m[1], m[2]
	what = "fetch " + repo + " " + op
	id, err := newRequestID()
	if err != nil {
		refuse(http.StatusInternalServerError, "split-gh: cannot generate an audit request ID")
		return
	}
	if err := q.audit(map[string]any{"phase": "start", "service": "git", "request_id": id, "method": r.Method, "repo": repo, "op": op}); err != nil {
		refuse(http.StatusInternalServerError, "split-gh: "+auditError(err).Error())
		return
	}
	defer func() {
		_ = q.audit(map[string]any{"phase": "finish", "service": "git", "request_id": id, "outcome": outcome})
	}()

	var query string
	switch {
	case op == "git-receive-pack" || r.URL.Query().Get("service") == "git-receive-pack":
		what = "push to " + repo
		refuse(http.StatusForbidden, pushRefusal)
		return
	case op == "info/refs" && r.Method == http.MethodGet && r.URL.RawQuery == "service=git-upload-pack":
		query = "?service=git-upload-pack"
	case op == "git-upload-pack" && r.Method == http.MethodPost:
	default:
		refuse(http.StatusBadRequest, "split-gh: only git's smart HTTP protocol is supported")
		return
	}
	if !readpolicy.Allowed(repo, q.conf.ReadRepos) {
		refuse(http.StatusForbidden, "split-gh: this qube may not read "+repo+"; the user can add it to read_repos in gh-auth")
		return
	}

	var body io.Reader
	if r.Method == http.MethodPost {
		b, err := readBody(w, r, maxGitRequest)
		if err != nil {
			refuse(http.StatusBadRequest, "split-gh: can't read the request: "+err.Error())
			return
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(r.Context(), gitDeadline)
	defer cancel()
	up, err := http.NewRequestWithContext(ctx, r.Method, q.github+"/"+repo+".git/"+op+query, body)
	if err != nil {
		refuse(http.StatusInternalServerError, "split-gh: "+err.Error())
		return
	}
	// Only what git's protocol needs goes to GitHub. The password stays here.
	for _, h := range []string{"Accept", "Content-Type", "Content-Encoding", "Git-Protocol"} {
		if v := r.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	agent := r.Header.Get("User-Agent")
	if !strings.HasPrefix(agent, "git/") || len(agent) > 100 {
		agent = "git/split-gh"
	}
	up.Header.Set("User-Agent", agent)
	up.SetBasicAuth("x-access-token", q.token)
	resp, err := q.client.Do(up)
	if err != nil {
		refuse(http.StatusBadGateway, "split-gh: can't reach GitHub: "+err.Error())
		return
	}
	defer resp.Body.Close()

	want := "application/x-git-upload-pack-result"
	if op == "info/refs" {
		want = "application/x-git-upload-pack-advertisement"
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		refuse(http.StatusNotFound, "split-gh: GitHub has no repository "+repo+" that gh-auth's read token can read")
		return
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		refuse(http.StatusForbidden, "split-gh: GitHub refused gh-auth's read token for "+repo+": "+resp.Status)
		return
	case resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != want:
		refuse(http.StatusBadGateway, fmt.Sprintf("split-gh: unexpected answer from GitHub for %s: %s, %q", repo, resp.Status, resp.Header.Get("Content-Type")))
		return
	}
	w.Header().Set("Content-Type", want)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	idle := time.AfterFunc(gitIdle, cancel)
	defer idle.Stop()
	n, err := relay(w, http.NewResponseController(w), resp.Body, idle)
	outcome = fmt.Sprintf("%d KB", n>>10)
	if err != nil {
		outcome += ", cut off: " + err.Error()
	}
}

// relay copies GitHub's answer to the client. A minute without data ends
// it: the timer cancels the request to GitHub, and the write deadline ends
// a write that the client doesn't read.
func relay(w http.ResponseWriter, rc *http.ResponseController, from io.Reader, idle *time.Timer) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := from.Read(buf)
		if n > 0 {
			idle.Reset(gitIdle)
			_ = rc.SetWriteDeadline(time.Now().Add(gitIdle))
			if _, err := w.Write(buf[:n]); err != nil {
				return total, err
			}
			if err := rc.Flush(); err != nil {
				return total, err
			}
			total += int64(n)
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// githubClient connects to GitHub directly, as gh does, and returns
// redirects instead of following them.
func githubClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ResponseHeaderTimeout = gitIdle
	t.DisableCompression = true
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
