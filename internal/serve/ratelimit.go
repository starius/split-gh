package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const readsPerMinute = 60

// admitRead shares a rolling per-caller limit across qrexec processes.
func admitRead(home, caller string, now time.Time) (bool, error) {
	if home == "" {
		return false, fmt.Errorf("home directory unavailable")
	}
	dir := filepath.Join(home, ".local/state/split-gh/rate")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	sum := sha256.Sum256([]byte(caller))
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:])), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return false, err
	}
	var times []int64
	if len(b) > 0 && (len(b) > 16384 || json.Unmarshal(b, &times) != nil) {
		return false, fmt.Errorf("invalid rate state")
	}
	kept := times[:0]
	for _, t := range times {
		if t > now.Add(-time.Minute).UnixNano() && t <= now.Add(time.Minute).UnixNano() {
			kept = append(kept, t)
		}
	}
	if len(kept) >= readsPerMinute {
		return false, nil
	}
	kept = append(kept, now.UnixNano())
	data, err := json.Marshal(kept)
	if err != nil {
		return false, err
	}
	if err = f.Truncate(0); err != nil {
		return false, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	if _, err = f.Write(data); err != nil {
		return false, err
	}
	return true, nil
}
