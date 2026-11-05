package wire

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestDecodeRequest(t *testing.T) {
	for _, tc := range []struct {
		s  string
		ok bool
	}{
		{`{"v":1,"repo":"o/r"}` + "\n", true},
		{`{"v":1,"repo":"o/r"}`, true},
		{`{"v":1,"extra":1}`, false},
		{`{"v":2}`, false},
		{`{"v":1} {}`, false},
		{`{"v":1} x`, false},
		{`{"v":1,"argv":["` + strings.Repeat("x", MaxRequest) + `"]}`, false},
	} {
		_, e := DecodeRequest([]byte(tc.s))
		if (e == nil) != tc.ok {
			t.Errorf("%.40q: %v", tc.s, e)
		}
	}
}

func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte("{\"v\":1}\n"))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = DecodeRequest(b) })
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
	for _, f := range []struct {
		kind byte
		data string
	}{{'o', "out"}, {'e', "err"}, {'x', "\x03"}} {
		if err := Frame(&buf, f.kind, []byte(f.data)); err != nil {
			t.Fatal(err)
		}
	}
	if code, err := Receive(&buf, &out, &errout); err != nil || code != 3 || out.String() != "out" || errout.String() != "err" {
		t.Fatalf("code=%d stdout=%q stderr=%q err=%v", code, out.String(), errout.String(), err)
	}
	if err := Frame(shortWriter{}, 'o', []byte("hello")); err != io.ErrShortWrite {
		t.Fatalf("short write: %v", err)
	}
	if _, err := Receive(bytes.NewReader([]byte{'j', 0, 0, 0, 0}), io.Discard, io.Discard); err == nil {
		t.Fatal("unknown frame kind accepted")
	}
}
