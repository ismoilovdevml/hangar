package npm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// The publish body is a JSON packument whose _attachments carry tarballs as
// base64 strings. It is parsed by a small streaming scanner so that tarball
// data flows from the request straight into the blob store: only the other
// (metadata) fields are held in memory, bounded by maxMetadataSize.

// maxMetadataSize bounds the non-attachment part of a publish or update body.
const maxMetadataSize = 32 << 20

// errTooLarge marks a body whose metadata exceeds maxMetadataSize.
var errTooLarge = errors.New("request metadata too large")

// attachment is one entry of _attachments after streaming.
type attachment struct {
	Name        string
	ContentType string
	Length      int64 // declared "length"; -1 if absent
	Result      any   // what onData returned; nil if data was skipped
}

// publishBody is a parsed publish/update request.
type publishBody struct {
	Fields      map[string]json.RawMessage // every top-level field except _attachments
	Attachments []*attachment
}

// onDataFunc consumes one attachment's decoded content. It is called only
// for attachments whose name ends in ".tgz"; r yields the base64-decoded bytes.
type onDataFunc func(name string, r io.Reader) (any, error)

type scanner struct {
	r      *bufio.Reader
	budget int64
}

func syntaxErr(msg string) error {
	return fmt.Errorf("malformed JSON body: %s: %w", msg, core.ErrInvalid)
}

func (s *scanner) byte() (byte, error) {
	c, err := s.r.ReadByte()
	if err == io.EOF {
		return 0, syntaxErr("unexpected end of input")
	}
	return c, err
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// next returns the next non-whitespace byte.
func (s *scanner) next() (byte, error) {
	for {
		c, err := s.byte()
		if err != nil || !isSpace(c) {
			return c, err
		}
	}
}

func (s *scanner) expect(want byte) error {
	c, err := s.next()
	if err != nil {
		return err
	}
	if c != want {
		return syntaxErr(fmt.Sprintf("expected %q, got %q", want, c))
	}
	return nil
}

func (s *scanner) put(buf *bytes.Buffer, c byte) error {
	s.budget--
	if s.budget < 0 {
		return fmt.Errorf("%w: %w", errTooLarge, core.ErrInvalid)
	}
	return buf.WriteByte(c)
}

// captureString copies a string body (opening quote already consumed and
// written) including the closing quote.
func (s *scanner) captureString(buf *bytes.Buffer) error {
	for {
		c, err := s.byte()
		if err != nil {
			return err
		}
		if err := s.put(buf, c); err != nil {
			return err
		}
		switch c {
		case '"':
			return nil
		case '\\':
			e, err := s.byte()
			if err != nil {
				return err
			}
			if err := s.put(buf, e); err != nil {
				return err
			}
		}
	}
}

// capture copies one JSON value starting with first into buf.
func (s *scanner) capture(first byte, buf *bytes.Buffer) error {
	if err := s.put(buf, first); err != nil {
		return err
	}
	switch first {
	case '"':
		return s.captureString(buf)
	case '{', '[':
		depth := 1
		for depth > 0 {
			c, err := s.byte()
			if err != nil {
				return err
			}
			if err := s.put(buf, c); err != nil {
				return err
			}
			switch c {
			case '"':
				if err := s.captureString(buf); err != nil {
					return err
				}
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		return nil
	default: // number, true, false, null
		for {
			c, err := s.r.ReadByte()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if isSpace(c) || c == ',' || c == '}' || c == ']' {
				return s.r.UnreadByte()
			}
			if err := s.put(buf, c); err != nil {
				return err
			}
		}
	}
}

// key reads an object key (or reports the end of the object).
func (s *scanner) key(first bool) (string, bool, error) {
	c, err := s.next()
	if err != nil {
		return "", false, err
	}
	if !first {
		switch c {
		case '}':
			return "", true, nil
		case ',':
			if c, err = s.next(); err != nil {
				return "", false, err
			}
		default:
			return "", false, syntaxErr("expected ',' or '}'")
		}
	} else if c == '}' {
		return "", true, nil
	}
	if c != '"' {
		return "", false, syntaxErr("expected object key")
	}
	var buf bytes.Buffer
	if err := s.capture('"', &buf); err != nil {
		return "", false, err
	}
	var k string
	if err := json.Unmarshal(buf.Bytes(), &k); err != nil {
		return "", false, syntaxErr("bad key")
	}
	if err := s.expect(':'); err != nil {
		return "", false, err
	}
	return k, false, nil
}

// stringReader streams the content of a JSON string (opening quote already
// consumed) and stops at the closing quote. Only the escapes that can occur
// in base64 text are accepted.
type stringReader struct {
	s    *scanner
	done bool
}

func (sr *stringReader) Read(p []byte) (int, error) {
	if sr.done {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		if n > 0 && sr.s.r.Buffered() == 0 {
			break
		}
		c, err := sr.s.byte()
		if err != nil {
			return n, err
		}
		switch c {
		case '"':
			sr.done = true
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		case '\\':
			e, err := sr.s.byte()
			if err != nil {
				return n, err
			}
			switch e {
			case '/', '\\', '"':
				c = e
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			default:
				return n, syntaxErr("unsupported escape in attachment data")
			}
		}
		p[n] = c
		n++
	}
	return n, nil
}

func (s *scanner) attachments(onData onDataFunc) ([]*attachment, error) {
	if err := s.expect('{'); err != nil {
		return nil, err
	}
	var out []*attachment
	for first := true; ; first = false {
		name, end, err := s.key(first)
		if err != nil {
			return nil, err
		}
		if end {
			return out, nil
		}
		a := &attachment{Name: name, Length: -1}
		if err := s.expect('{'); err != nil {
			return nil, err
		}
		for afirst := true; ; afirst = false {
			k, aend, err := s.key(afirst)
			if err != nil {
				return nil, err
			}
			if aend {
				break
			}
			c, err := s.next()
			if err != nil {
				return nil, err
			}
			if k == "data" && strings.HasSuffix(name, ".tgz") {
				if c != '"' {
					return nil, syntaxErr("attachment data must be a string")
				}
				sr := &stringReader{s: s}
				res, err := onData(name, sr)
				if err != nil {
					return nil, err
				}
				if _, err := io.Copy(io.Discard, sr); err != nil {
					return nil, err
				}
				a.Result = res
				continue
			}
			var buf bytes.Buffer
			if err := s.capture(c, &buf); err != nil {
				return nil, err
			}
			switch k {
			case "content_type":
				_ = json.Unmarshal(buf.Bytes(), &a.ContentType)
			case "length":
				if err := json.Unmarshal(buf.Bytes(), &a.Length); err != nil {
					return nil, syntaxErr("attachment length must be a number")
				}
			}
		}
		out = append(out, a)
	}
}

// parsePublishBody reads a publish or packument-update body.
func parsePublishBody(r io.Reader, onData onDataFunc) (*publishBody, error) {
	s := &scanner{r: bufio.NewReaderSize(r, 64<<10), budget: maxMetadataSize}
	if err := s.expect('{'); err != nil {
		return nil, err
	}
	b := &publishBody{Fields: map[string]json.RawMessage{}}
	for first := true; ; first = false {
		k, end, err := s.key(first)
		if err != nil {
			return nil, err
		}
		if end {
			break
		}
		if k == "_attachments" {
			if b.Attachments, err = s.attachments(onData); err != nil {
				return nil, err
			}
			continue
		}
		c, err := s.next()
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := s.capture(c, &buf); err != nil {
			return nil, err
		}
		if !json.Valid(buf.Bytes()) {
			return nil, syntaxErr(fmt.Sprintf("invalid value for %q", k))
		}
		b.Fields[k] = buf.Bytes()
	}
	if c, err := s.next(); err == nil {
		return nil, syntaxErr(fmt.Sprintf("trailing data %q", c))
	}
	return b, nil
}
