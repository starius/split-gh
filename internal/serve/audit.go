package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// audit appends a record about the qube to the audit log.
func (q *qube) audit(event map[string]any) error {
	event["time"] = time.Now().UTC()
	event["caller"] = q.name
	return appendAudit(q.home, event)
}

// auditExecution records a gh command line before it runs.
func (q *qube) auditExecution(id, repo string, args []string) error {
	if err := q.audit(map[string]any{"phase": "execute", "request_id": id, "repo": repo, "argv": args}); err != nil {
		return auditError(err)
	}
	return nil
}

// maxAuditBytes caps the audit log. Past it, serve refuses requests rather
// than drop records; the user moves the log aside to resume.
const maxAuditBytes = 100 << 20

var errAuditFull = errors.New("audit log is over 100 MB; in gh-auth, move ~/.local/state/split-gh/log.jsonl aside")

// auditError is what the caller sees when a record can't be written.
func auditError(err error) error {
	if errors.Is(err, errAuditFull) {
		return err
	}
	return errors.New("audit log unavailable")
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
			args := make([]string, 0, len(v))
			budget := 4096
			for _, arg := range v {
				if budget <= 0 || len(args) == 32 {
					args = append(args, "...[truncated]")
					break
				}
				arg = auditString(arg)
				budget -= len(arg)
				args = append(args, arg)
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
	if fi, err := f.Stat(); err != nil || fi.Size() > maxAuditBytes {
		_ = f.Close()
		if err != nil {
			return err
		}
		return errAuditFull
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(event); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
