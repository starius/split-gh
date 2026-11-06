// Package serve is split-gh serve, which the user runs in gh-auth. It
// answers the agent qubes, each on its own port on 127.0.0.1, which dom0's
// qubes.ConnectTCP policy lets only that qube reach. It only reads GitHub:
// gh with a read-only token, and git fetches.
package serve

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"split-gh/internal/config"
	"split-gh/internal/wire"
)

const (
	ghPerMinute = 60
	// ghSlots bounds concurrent gh runs; each one takes about 40 MB.
	ghSlots = 8
	// A fetch is a few requests, after a question.
	gitPerMinute = 120
	gitSlots     = 4
	// minPassword is the shortest password serve accepts.
	minPassword = 16
)

// bodyTimeout bounds reading a request body, and writeTimeout each write of
// an answer. Tests shorten them.
var (
	bodyTimeout  = 30 * time.Second
	writeTimeout = 30 * time.Second
)

// daemon is what the qubes' handlers share.
type daemon struct {
	home   string
	token  string // GitHub read token
	github string // https://github.com; tests use a local server
	client *http.Client

	outMu sync.Mutex
	out   io.Writer // the user's terminal
}

// qube answers one agent qube.
type qube struct {
	*daemon
	name     string
	conf     config.Qube
	password [sha256.Size]byte // SHA-256 of the qube's password
	gh, git  *limiter
}

// Main is split-gh serve. It answers until Ctrl-C.
func Main(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: split-gh serve")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "split-gh serve:", err)
		return 1
	}
	qubes, err := load(home, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "split-gh serve:", err)
		return 1
	}
	// Ctrl-C cancels this context, which every request's derives from: it
	// kills gh and ends git fetches.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var servers []*http.Server
	errs := make(chan error, len(qubes))
	for _, q := range qubes {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(q.conf.Port))
		if err != nil {
			fmt.Fprintln(os.Stderr, "split-gh serve:", err)
			return 1
		}
		s := &http.Server{
			Handler:           q,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       time.Minute,
			MaxHeaderBytes:    64 << 10,
			BaseContext:       func(net.Listener) context.Context { return ctx },
		}
		defer s.Close()
		servers = append(servers, s)
		go func() { errs <- s.Serve(l) }()
		q.printf("answering on 127.0.0.1:%d; GitHub repositories it may read: %s",
			q.conf.Port, strings.Join(q.conf.ReadRepos, " "))
	}
	fmt.Fprintln(os.Stderr, "Ctrl-C stops split-gh serve and ends all access.")
	select {
	case <-ctx.Done():
		// The requests are cancelled already; wait a little for them to end.
		wait, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(wait)
		}
		fmt.Fprintln(os.Stderr, "split-gh serve: stopped")
		return 0
	case err := <-errs:
		fmt.Fprintln(os.Stderr, "split-gh serve:", err)
		return 1
	}
}

// load reads the config, the token and the passwords.
func load(home string, out io.Writer) ([]*qube, error) {
	cfg, err := config.Load(home)
	if err != nil {
		return nil, fmt.Errorf("~/.config/split-gh/config.json: %w", err)
	}
	token, err := readSecret(config.Path(home, cfg.ReadTokenFile))
	if err != nil {
		return nil, fmt.Errorf("read_token_file: %w", err)
	}
	d := &daemon{home: home, token: token, github: "https://github.com", client: githubClient(), out: out}
	names := make([]string, 0, len(cfg.Qubes))
	for name := range cfg.Qubes {
		names = append(names, name)
	}
	sort.Strings(names)
	ports := map[int]string{}
	var qubes []*qube
	for _, name := range names {
		c := cfg.Qubes[name]
		if c.Port < 1024 || c.Port > 65535 {
			return nil, fmt.Errorf("qube %s: port must be from 1024 to 65535", name)
		}
		if other, taken := ports[c.Port]; taken {
			return nil, fmt.Errorf("qubes %s and %s have the same port", other, name)
		}
		ports[c.Port] = name
		password, err := readSecret(config.Path(home, c.PasswordFile))
		if err != nil {
			return nil, fmt.Errorf("qube %s: password_file: %w", name, err)
		}
		if len(password) < minPassword {
			return nil, fmt.Errorf("qube %s: the password must have at least %d characters", name, minPassword)
		}
		qubes = append(qubes, &qube{daemon: d, name: name, conf: c, password: sha256.Sum256([]byte(password)),
			gh: newLimiter(ghPerMinute, ghSlots), git: newLimiter(gitPerMinute, gitSlots)})
	}
	if len(qubes) == 0 {
		return nil, errors.New("config.json has no qubes")
	}
	return qubes, nil
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

// ServeHTTP checks the limits and the password, then answers a gh read
// (POST /gh), the question before a git fetch (wire.FetchesPath) or a git
// fetch (any other path).
func (q *qube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A client that stops reading can't hold a request, and its slot: every
	// write has a deadline. Answers that stream move it on with each write.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	// A body must arrive in time, also one that no handler reads: the
	// server reads what's left of it before answering. readBody lifts the
	// deadline once the body is in.
	if r.ContentLength != 0 {
		_ = rc.SetReadDeadline(time.Now().Add(bodyTimeout))
	}
	isGH := r.URL.Path == "/gh"
	l := q.git
	if isGH {
		l = q.gh
	}
	// Requests over the limits aren't logged, so they can't fill the log.
	release, refusal := l.take(time.Now())
	if refusal != "" {
		fail(w, refusal, http.StatusTooManyRequests)
		return
	}
	defer release()
	// Never 401: git would ask for a password.
	if !q.authorized(r) {
		q.printf("refused a request with a wrong password")
		fail(w, "split-gh: wrong password; the agent qube has it in ~/.config/split-gh/password", http.StatusForbidden)
		return
	}
	if isGH {
		q.serveGH(w, r)
	} else if repo, ok := strings.CutPrefix(r.URL.Path, wire.FetchesPath); ok {
		q.answerFetches(w, r, repo)
	} else {
		q.serveGit(w, r)
	}
}

// fail answers with an error text. Like every write, it has a deadline,
// counted from now: a refusal can come late, after GitHub answered.
func fail(w http.ResponseWriter, msg string, status int) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeTimeout))
	http.Error(w, msg, status)
}

// readBody reads a request body of at most limit bytes, and then lifts the
// read deadline, which would otherwise cancel a long request. After an
// error the connection closes, since the rest of the body is unread.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		w.Header().Set("Connection", "close")
		return nil, err
	}
	return b, http.NewResponseController(w).SetReadDeadline(time.Time{})
}

// authorized checks the header "Authorization: Bearer PASSWORD".
func (q *qube) authorized(r *http.Request) bool {
	password, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	sum := sha256.Sum256([]byte(password))
	return ok && subtle.ConstantTimeCompare(sum[:], q.password[:]) == 1
}

// printf writes a line about the qube to the user's terminal.
func (q *qube) printf(format string, args ...any) {
	line := terminalSafe(fmt.Sprintf(format, args...))
	if len(line) > 500 {
		line = line[:500] + "..."
	}
	q.outMu.Lock()
	defer q.outMu.Unlock()
	fmt.Fprintf(q.out, "%s %s: %s\n", time.Now().Format("15:04:05"), q.name, line)
}

// terminalSafe replaces everything but printable ASCII, so text the agent
// chose can't send escape sequences to the terminal.
func terminalSafe(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 0x20 && c < 0x7f {
			b.WriteRune(c)
		} else {
			fmt.Fprintf(&b, "<U+%04X>", c)
		}
	}
	return b.String()
}

// show formats a command line for the terminal.
func show(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \"'\\") {
			a = strconv.Quote(a)
		}
		quoted[i] = a
	}
	return strings.Join(quoted, " ")
}
