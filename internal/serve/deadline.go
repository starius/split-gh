package serve

import (
	"io"
	"os"
	"time"
)

// ReadService enforces a process-wide limit even when a qrexec pipe blocks.
func ReadService(in io.Reader, out io.Writer, deadline time.Duration) {
	timer := time.AfterFunc(deadline, func() {
		done := make(chan struct{})
		go func() {
			home, _ := os.UserHomeDir()
			_ = appendAudit(home, map[string]any{"time": time.Now().UTC(), "phase": "timeout", "caller": os.Getenv("QREXEC_REMOTE_DOMAIN")})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
		}
		os.Exit(1)
	})
	defer timer.Stop()
	Read(in, out)
}
