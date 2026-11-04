package wire

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestHeaders(t *testing.T) {
	for _, tc := range []struct {
		s  string
		ok bool
	}{{`{"v":1,"repo":"o/r"}` + "\n", true}, {`{"v":1,"extra":1}` + "\n", false}, {`{"v":2}` + "\n", false}, {strings.Repeat("x", MaxHeader+1) + "\n", false}} {
		_, e := ReadRequest(strings.NewReader(tc.s))
		if (e == nil) != tc.ok {
			t.Errorf("header: %v", e)
		}
	}
}
func FuzzHeader(f *testing.F) {
	f.Add("{\"v\":1}\n")
	f.Fuzz(func(t *testing.T, s string) { ReadRequest(strings.NewReader(s)) })
}

func FuzzReceive(f *testing.F) {
	f.Add([]byte{'x', 0, 0, 0, 1, 0})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = Receive(bytes.NewReader(data), io.Discard, io.Discard) })
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}
func TestFrames(t *testing.T) {
	var buf, out, errout bytes.Buffer
	if err := Frame(&buf, 'j', []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := Frame(&buf, 'x', []byte{0}); err != nil {
		t.Fatal(err)
	}
	if code, err := Receive(&buf, &out, &errout); err != nil || code != 0 || out.String() != `{"ok":true}` {
		t.Fatalf("code=%d output=%q err=%v", code, out.String(), err)
	}
	if err := Frame(shortWriter{}, 'o', []byte("hello")); err != io.ErrShortWrite {
		t.Fatalf("short write: %v", err)
	}
}

func TestHeaderLeavesPayloadUnread(t *testing.T) {
	r := bytes.NewBufferString("{\"v\":1}\n\x00payload")
	if _, err := ReadRequest(r); err != nil {
		t.Fatal(err)
	}
	if got := r.String(); got != "\x00payload" {
		t.Fatalf("remaining payload %q", got)
	}
}
