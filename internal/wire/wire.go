package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const MaxRequest = 64 << 10
const MaxOutput = 16 << 20

// DecodeRequest strictly decodes the body of POST /gh: one JSON object with
// known fields only, and version 1.
func DecodeRequest(b []byte) (Request, error) {
	var q Request
	if len(b) > MaxRequest {
		return q, errors.New("request too large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&q); err != nil {
		return q, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return q, errors.New("trailing data")
	}
	if q.V != 1 {
		return q, errors.New("unsupported version")
	}
	return q, nil
}

// Frame writes one frame: its kind, its length and the bytes.
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

// Receive copies the frames of an answer to out and errout, and returns the
// exit status.
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
		case 'o', 'e':
			w := out
			if h[0] == 'e' {
				w = errout
			}
			if n, err := w.Write(b); err != nil {
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
