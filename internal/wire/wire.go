package wire

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const MaxHeader = 1 << 20
const MaxOutput = 16 << 20

func ReadRequest(r io.Reader) (Request, error) {
	var q Request
	b := bufio.NewReader(io.LimitReader(r, MaxHeader+1))
	line, e := b.ReadBytes('\n')
	if e != nil || len(line) > MaxHeader {
		return q, errors.New("invalid or oversized header")
	}
	d := json.NewDecoder(bytesReader(line))
	d.DisallowUnknownFields()
	if e = d.Decode(&q); e != nil {
		return q, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return q, errors.New("trailing JSON")
	}
	if q.V != 1 {
		return q, errors.New("unsupported version")
	}
	return q, nil
}
func bytesReader(b []byte) io.Reader { return &reader{b: b} }

type reader struct{ b []byte }

func (r *reader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
func Frame(w io.Writer, k byte, b []byte) error {
	var h [5]byte
	h[0] = k
	binary.BigEndian.PutUint32(h[1:], uint32(len(b)))
	if n, e := w.Write(h[:]); e != nil {
		return e
	} else if n != len(h) {
		return io.ErrShortWrite
	}
	if n, e := w.Write(b); e != nil {
		return e
	} else if n != len(b) {
		return io.ErrShortWrite
	}
	return nil
}
func Receive(r io.Reader, out, errout io.Writer) (int, error) {
	var total uint64
	for {
		var h [5]byte
		if _, e := io.ReadFull(r, h[:]); e != nil {
			return 1, e
		}
		n := binary.BigEndian.Uint32(h[1:])
		total += uint64(n)
		if total > MaxOutput {
			return 1, errors.New("response too large")
		}
		b := make([]byte, n)
		if _, e := io.ReadFull(r, b); e != nil {
			return 1, e
		}
		switch h[0] {
		case 'o', 'j':
			if h[0] == 'j' && !json.Valid(b) {
				return 1, errors.New("invalid JSON frame")
			}
			if n, err := out.Write(b); err != nil {
				return 1, err
			} else if n != len(b) {
				return 1, io.ErrShortWrite
			}
		case 'e':
			if n, err := errout.Write(b); err != nil {
				return 1, err
			} else if n != len(b) {
				return 1, io.ErrShortWrite
			}
		case 'x':
			if len(b) != 1 {
				return 1, errors.New("invalid exit status")
			}
			return int(b[0]), nil
		default:
			return 1, errors.New("invalid frame")
		}
	}
}
