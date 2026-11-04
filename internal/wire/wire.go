package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const MaxHeader = 64 << 10
const MaxOutput = 16 << 20

func ReadRequest(r io.Reader) (Request, error) {
	var q Request
	// Read only the header so any following binary payload remains available.
	line := make([]byte, 0, 1024)
	var one [1]byte
	for len(line) < MaxHeader {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return q, errors.New("invalid or oversized header")
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			break
		}
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return q, errors.New("invalid or oversized header")
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if e := d.Decode(&q); e != nil {
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
