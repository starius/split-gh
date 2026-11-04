package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"split-gh/internal/readpolicy"
	"split-gh/internal/wire"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	Qubes map[string]struct {
		ReadRepos []string `json:"read_repos"`
	} `json:"qubes"`
	ReadTokenFile string `json:"read_token_file"`
}

func Read(in io.Reader, out io.Writer) {
	setOutputDeadline(out, 75*time.Second)
	code := byte(1)
	var q wire.Request
	caller := os.Getenv("QREXEC_REMOTE_DOMAIN")
	home, _ := os.UserHomeDir()
	started := false
	var requestID string
	defer func() {
		phase := "rejected"
		if started {
			phase = "finish"
		}
		_ = appendAudit(home, map[string]any{"time": time.Now().UTC(), "phase": phase, "request_id": requestID, "caller": caller, "repo": q.Repo, "argv": q.Argv, "status": code})
		wire.Frame(out, 'x', []byte{code})
	}()
	fail := func(e error) { wire.Frame(out, 'e', []byte(e.Error()+"\n")) }
	q, e := readHeader(in, 10*time.Second)
	if e != nil {
		fail(e)
		return
	}
	home, e = os.UserHomeDir()
	if e != nil {
		fail(e)
		return
	}
	var id [16]byte
	if _, e = rand.Read(id[:]); e != nil {
		fail(fmt.Errorf("cannot generate audit request ID"))
		return
	}
	requestID = hex.EncodeToString(id[:])
	if e = appendAudit(home, map[string]any{"time": time.Now().UTC(), "phase": "start", "request_id": requestID, "caller": caller, "repo": q.Repo, "argv": q.Argv}); e != nil {
		fail(fmt.Errorf("audit log unavailable"))
		return
	}
	started = true
	b, e := os.ReadFile(filepath.Join(home, ".config/split-gh/config.json"))
	if e != nil {
		fail(e)
		return
	}
	var c config
	if e = json.Unmarshal(b, &c); e != nil {
		fail(e)
		return
	}
	rights, ok := c.Qubes[caller]
	if !ok || caller == "" {
		fail(fmt.Errorf("caller not configured"))
		return
	}
	var args []string
	var threadNumber int
	normalized, err := readpolicy.NormalizeGlobal(q.Argv)
	if err != nil {
		fail(err)
		return
	}
	if len(normalized) >= 2 && normalized[0] == "pr" && normalized[1] == "threads" {
		threadNumber, err = parseThreadArgs(normalized[2:], q.Repo, rights.ReadRepos)
		if err != nil {
			fail(err)
			return
		}
	} else {
		args, e = readpolicy.Parse(q.Argv, q.Repo, q.Branch, rights.ReadRepos)
		if e != nil {
			fail(e)
			return
		}
	}

	tokenPath := c.ReadTokenFile
	if strings.HasPrefix(tokenPath, "~/") {
		tokenPath = filepath.Join(home, tokenPath[2:])
	}
	token, e := os.ReadFile(tokenPath)
	if e != nil {
		fail(fmt.Errorf("cannot read token"))
		return
	}
	tmp, e := os.MkdirTemp("", "split-gh-read-")
	if e != nil {
		fail(e)
		return
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + tmp, "GH_CONFIG_DIR=" + tmp, "GH_TOKEN=" + strings.TrimSpace(string(token)), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_PAGER=cat", "NO_COLOR=1"}
	stream := &outputStream{out: out, cancel: cancel}
	if threadNumber != 0 {
		fetch := func(query string, fields ...string) ([]byte, error) {
			args, err := threadQueryArgs(query, fields...)
			if err != nil {
				return nil, err
			}
			if err := auditExecution(home, requestID, caller, q.Repo, args); err != nil {
				return nil, err
			}
			cmd := exec.CommandContext(ctx, "gh", args...)
			cmd.Dir = tmp
			cmd.Env = env
			var b boundedBuffer
			cmd.Stdout = &b
			cmd.Stderr = frameWriter{stream, 'e'}
			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("fetch review threads: %w", err)
			}
			return b.Bytes(), nil
		}
		parts := strings.Split(q.Repo, "/")
		if err := writeThreads(fetch, frameWriter{stream, 'o'}, parts[0], parts[1], threadNumber); err != nil {
			fail(err)
			return
		}
		code = 0
		return
	}

	// gh normally prompts for a run when no ID or job is supplied. Since the
	// service is noninteractive, select the newest run for the checkout branch.
	if needsLatestRun(args) {
		listArgs := latestRunArgs(q.Repo, q.Branch)
		if err := auditExecution(home, requestID, caller, q.Repo, listArgs); err != nil {
			fail(err)
			return
		}
		cmd := exec.CommandContext(ctx, "gh", listArgs...)
		cmd.Dir = tmp
		cmd.Env = env
		var b boundedBuffer
		cmd.Stdout = &b
		cmd.Stderr = frameWriter{stream, 'e'}
		if err := cmd.Run(); err != nil {
			fail(fmt.Errorf("list workflow runs: %w", err))
			return
		}
		id, err := latestRunID(b.Bytes())
		if err != nil {
			fail(err)
			return
		}
		args = append(args[:2], append([]string{strconv.FormatUint(id, 10)}, args[2:]...)...)
	}
	if err := auditExecution(home, requestID, caller, q.Repo, args); err != nil {
		fail(err)
		return
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = tmp
	cmd.Env = env
	cmd.Stdout = frameWriter{stream, 'o'}
	cmd.Stderr = frameWriter{stream, 'e'}
	e = cmd.Run()
	if stream.overflow {
		fail(fmt.Errorf("output limit exceeded"))
		return
	}
	if ctx.Err() != nil {
		fail(fmt.Errorf("request timed out"))
		return
	}
	if e == nil {
		code = 0
	} else if x, ok := e.(*exec.ExitError); ok && x.ExitCode() > 0 && x.ExitCode() < 256 {
		code = byte(x.ExitCode())
	} else {
		fail(e)
	}

}

type outputStream struct {
	mu       sync.Mutex
	out      io.Writer
	cancel   context.CancelFunc
	total    int
	overflow bool
}
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
	if err := wire.Frame(s.out, w.kind, p); err != nil {
		s.cancel()
		return 0, err
	}
	s.total += len(p)
	return len(p), nil
}

// A qrexec caller can leave stdin open without sending a complete header.
// The service must still terminate within a bounded time.
func readHeader(in io.Reader, timeout time.Duration) (wire.Request, error) {
	type result struct {
		request wire.Request
		err     error
	}
	done := make(chan result, 1)
	go func() { q, err := wire.ReadRequest(in); done <- result{q, err} }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.request, r.err
	case <-timer.C:
		return wire.Request{}, fmt.Errorf("request header timed out")
	}
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

func auditExecution(home, id, caller, repo string, args []string) error {
	if err := appendAudit(home, map[string]any{"time": time.Now().UTC(), "phase": "execute", "request_id": id, "caller": caller, "repo": repo, "argv": args}); err != nil {
		return fmt.Errorf("audit log unavailable")
	}
	return nil
}

func auditString(s string) string {
	const limit = 256
	if len(s) > limit {
		return s[:limit] + "...[truncated]"
	}
	return s
}

func appendAudit(home string, event map[string]any) error {
	// Bound each record even when all input fields are controlled by the caller.
	for key, value := range event {
		switch v := value.(type) {
		case string:
			event[key] = auditString(v)
		case []string:
			n := len(v)
			if n > 32 {
				n = 32
			}
			args := make([]string, 0, n+1)
			for _, arg := range v[:n] {
				args = append(args, auditString(arg))
			}
			if len(v) > n {
				args = append(args, "...[truncated]")
			}
			event[key] = args
		}
	}

	if home == "" {
		return fmt.Errorf("home directory unavailable")
	}
	dir := filepath.Join(home, ".local/state/split-gh")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(f).Encode(event); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// qrexec stdout is a pipe or socket. Bound writes as well as the child process:
// an untrusted caller can stop reading while keeping its end open.
func setOutputDeadline(out io.Writer, duration time.Duration) {
	if w, ok := out.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = w.SetWriteDeadline(time.Now().Add(duration))
	}
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
