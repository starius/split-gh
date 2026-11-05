package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"split-gh/internal/readpolicy"
	"split-gh/internal/wire"
)

// ghTimeout bounds all the gh runs of one request.
const ghTimeout = 60 * time.Second

// serveGH answers POST /gh: one gh read, described by a wire.Request. A
// refusal is an HTTP error with the reason as text. Once gh runs, the answer
// is 200 with frames: gh's output, then its exit status.
func (q *qube) serveGH(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var req wire.Request
	outcome := ""
	defer func() {
		q.printf("gh %s -> %s (%.1fs)", show(req.Argv), outcome, time.Since(started).Seconds())
	}()
	refuse := func(status int, msg string) {
		outcome = "refused: " + msg
		fail(w, msg, status)
	}
	if r.Method != http.MethodPost {
		refuse(http.StatusMethodNotAllowed, "use POST")
		return
	}
	// Reading the whole body also lets the server notice when the client
	// goes away, which cancels r.Context() and so kills gh.
	body, err := readBody(w, r, wire.MaxRequest)
	if err == nil {
		req, err = wire.DecodeRequest(body)
	}
	if err != nil {
		refuse(http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	id, err := newRequestID()
	if err != nil {
		refuse(http.StatusInternalServerError, "cannot generate an audit request ID")
		return
	}
	if err := q.audit(map[string]any{"phase": "start", "service": "gh", "request_id": id, "repo": req.Repo, "argv": req.Argv}); err != nil {
		refuse(http.StatusInternalServerError, auditError(err).Error())
		return
	}
	defer func() {
		_ = q.audit(map[string]any{"phase": "finish", "service": "gh", "request_id": id, "outcome": outcome})
	}()

	var args []string
	var threadNumber int
	normalized, err := readpolicy.NormalizeGlobal(req.Argv)
	if err == nil && len(normalized) >= 2 && normalized[0] == "pr" && normalized[1] == "threads" {
		threadNumber, err = parseThreadArgs(normalized[2:], req.Repo, q.conf.ReadRepos)
	} else if err == nil {
		args, err = readpolicy.Parse(req.Argv, req.Repo, req.Branch, q.conf.ReadRepos)
	}
	if err != nil {
		refuse(http.StatusForbidden, err.Error())
		return
	}
	tmp, err := os.MkdirTemp("", "split-gh-read-")
	if err != nil {
		refuse(http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(tmp)

	ctx, cancel := context.WithTimeout(r.Context(), ghTimeout)
	defer cancel()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	stream := &outputStream{out: w, rc: http.NewResponseController(w), cancel: cancel}
	code, err := q.runGH(ctx, stream, tmp, id, req, args, threadNumber)
	outcome = fmt.Sprintf("exit %d", code)
	if err != nil {
		outcome += ": " + err.Error()
		stream.send('e', []byte(err.Error()+"\n"))
	}
	stream.send('x', []byte{code})
}

// runGH runs gh for an allowed request and returns its exit status.
func (q *qube) runGH(ctx context.Context, stream *outputStream, tmp, id string, req wire.Request, args []string, threadNumber int) (byte, error) {
	gh := func(args []string, stdout io.Writer) error {
		if err := q.auditExecution(id, req.Repo, args); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "gh", args...)
		cmd.Dir = tmp
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + tmp, "GH_CONFIG_DIR=" + tmp, "GH_TOKEN=" + q.token,
			"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_PAGER=cat", "NO_COLOR=1"}
		cmd.Stdout = stdout
		cmd.Stderr = frameWriter{stream, 'e'}
		return cmd.Run()
	}
	if threadNumber != 0 {
		fetch := func(query string, fields ...string) ([]byte, error) {
			args, err := threadQueryArgs(query, fields...)
			if err != nil {
				return nil, err
			}
			var b boundedBuffer
			if err := gh(args, &b); err != nil {
				return nil, fmt.Errorf("fetch review threads: %w", err)
			}
			return b.Bytes(), nil
		}
		owner, name, _ := strings.Cut(req.Repo, "/")
		if err := writeThreads(fetch, frameWriter{stream, 'o'}, owner, name, threadNumber); err != nil {
			return 1, err
		}
		return 0, nil
	}

	// gh normally prompts for a run when no ID or job is supplied. Since the
	// service is noninteractive, select the newest run for the checkout branch.
	if needsLatestRun(args) {
		var b boundedBuffer
		if err := gh(latestRunArgs(req.Repo, req.Branch), &b); err != nil {
			return 1, fmt.Errorf("list workflow runs: %w", err)
		}
		run, err := latestRunID(b.Bytes())
		if err != nil {
			return 1, err
		}
		args = append(args[:2], append([]string{strconv.FormatUint(run, 10)}, args[2:]...)...)
	}
	err := gh(args, frameWriter{stream, 'o'})
	var exit *exec.ExitError
	switch {
	case stream.overflowed():
		return 1, errors.New("output limit exceeded")
	case ctx.Err() != nil:
		return 1, errors.New("request timed out")
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() < 256:
		return byte(exit.ExitCode()), nil
	}
	return 1, err
}

// outputStream sends gh's output to the client as frames.
type outputStream struct {
	mu       sync.Mutex
	out      io.Writer
	rc       *http.ResponseController // nil in tests
	cancel   context.CancelFunc       // stops gh
	total    int
	overflow bool
	broken   bool
}

// frame writes one frame. The caller holds s.mu.
func (s *outputStream) frame(kind byte, p []byte) error {
	if s.broken {
		return errors.New("client gone")
	}
	if s.rc != nil {
		_ = s.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	err := wire.Frame(s.out, kind, p)
	if err == nil && s.rc != nil {
		err = s.rc.Flush()
	}
	if err != nil {
		s.broken = true
		s.cancel()
	}
	return err
}

// send writes a frame of the service's own, such as the exit status.
func (s *outputStream) send(kind byte, p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.frame(kind, p)
}

func (s *outputStream) overflowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overflow
}

// frameWriter sends gh's stdout ('o') or stderr ('e').
type frameWriter struct {
	stream *outputStream
	kind   byte
}

func (w frameWriter) Write(p []byte) (int, error) {
	s := w.stream
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > wire.MaxOutput-4096-s.total {
		s.overflow = true
		s.cancel()
		return 0, fmt.Errorf("output limit exceeded")
	}
	if err := s.frame(w.kind, p); err != nil {
		return 0, err
	}
	s.total += len(p)
	return len(p), nil
}

func newRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// boundedBuffer limits any individual GitHub page before JSON decoding.
type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxThreadBytes-b.Len() {
		return 0, fmt.Errorf("GitHub response exceeds limit")
	}
	return b.Buffer.Write(p)
}

func needsLatestRun(args []string) bool {
	if len(args) < 2 || args[0] != "run" || args[1] != "view" {
		return false
	}
	for _, arg := range args[2:] {
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--job=") {
			return false
		}
	}
	return true
}

func parseThreadArgs(args []string, repo string, allowed []string) (int, error) {
	if !readpolicy.Allowed(repo, allowed) {
		return 0, fmt.Errorf("repository not allowed")
	}
	var number int
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--repo" || a == "-R" {
			i++
			if i >= len(args) {
				return 0, fmt.Errorf("missing repository")
			}
			a = "--repo=" + args[i]
		}
		if strings.HasPrefix(a, "-R") && len(a) > 2 {
			a = "--repo=" + strings.TrimPrefix(a[2:], "=")
		}
		if strings.HasPrefix(a, "--repo=") {
			value := strings.TrimPrefix(a, "--repo=")
			value, valid := readpolicy.CanonicalRepo(value)
			if !valid || !strings.EqualFold(value, repo) || !readpolicy.Allowed(value, allowed) {
				return 0, fmt.Errorf("repository mismatch")
			}
			continue
		}
		n, err := strconv.Atoi(a)
		if err != nil || n < 1 || strconv.Itoa(n) != a || number != 0 {
			return 0, fmt.Errorf("invalid thread request")
		}
		number = n
	}
	if number == 0 {
		return 0, fmt.Errorf("invalid thread request")
	}
	return number, nil
}

func latestRunArgs(repo, branch string) []string {
	args := []string{"run", "list", "--repo=" + repo, "--limit=1", "--json=databaseId"}
	if branch != "" {
		args = append(args, "--branch="+branch)
	}
	return args
}

func latestRunID(data []byte) (uint64, error) {
	var runs []struct {
		DatabaseID uint64 `json:"databaseId"`
	}
	if err := json.Unmarshal(data, &runs); err != nil || len(runs) != 1 || runs[0].DatabaseID == 0 {
		return 0, fmt.Errorf("no workflow run found for this branch")
	}
	return runs[0].DatabaseID, nil
}
