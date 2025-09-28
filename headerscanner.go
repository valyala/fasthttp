package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
)

type headerScanner struct {
	initialized bool

	b []byte
	r int

	key   []byte
	value []byte

	err error
}

func (s *headerScanner) next() bool {
	if !s.initialized {
		if bytes.HasPrefix(s.b, strCRLF) {
			s.r = 2
			return false
		}

		i := bytes.Index(s.b, strCRLFCRLF)
		if i < 0 {
			s.err = errNeedMore
			return false
		}
		i += 4

		s.b = s.b[:i]
		if len(s.b) > 0 && (s.b[0] == ' ' || s.b[0] == '\t') {
			s.err = errors.New("invalid headers, headers cannot start with space or tab")
			return false
		}

		s.initialized = true
	}

	kv, err := s.readContinuedLineSlice()
	if len(kv) == 0 {
		s.err = err
		return false
	}

	// Key ends at first colon.
	k, v, ok := bytes.Cut(kv, strColon)
	if !ok {
		s.err = fmt.Errorf("malformed MIME header line: %q", kv)
		return false
	}
	if !isValidHeaderKey(k) {
		s.err = fmt.Errorf("malformed MIME header line: %q", kv)
		return false
	}

	// Skip initial spaces in value.
	v = bytes.TrimLeft(v, " \t")

	s.key = k
	s.value = v

	if err != nil {
		s.err = err
		return false
	}

	return true
}

// readLine reads a line from b, starting at s.r, and returns it.
func (s *headerScanner) readLine() (line []byte) {
	searchStart := 0

	for {
		if i := bytes.IndexByte(s.b[s.r+searchStart:], '\n'); i >= 0 {
			i += searchStart
			line = s.b[s.r : s.r+i+1]
			s.r += i + 1
			break
		}

		searchStart = len(s.b) - s.r
	}

	if len(line) == 0 {
		return nil
	}

	// drop \n and possible preceding \r
	if line[len(line)-1] == '\n' {
		drop := 1
		if len(line) > 1 && line[len(line)-2] == '\r' {
			drop = 2
		}
		line = line[:len(line)-drop]
	}
	return line
}

// readContinuedLineSlice reads continued lines from b until it finds a line
// that does not start with a space or tab, or it reaches the end of b.
func (s *headerScanner) readContinuedLineSlice() ([]byte, error) {
	line := s.readLine()
	if len(line) == 0 { // blank line - no continuation
		return line, nil
	}

	if bytes.IndexByte(line, ':') < 0 {
		return nil, fmt.Errorf("malformed MIME header: missing colon: %q", line)
	}

	// If the line doesn't start with a space or tab, we are done.
	if len(s.b)-s.r > 1 {
		peek := s.b[s.r : s.r+2]
		if len(peek) > 0 && (isASCIILetter(peek[0]) || peek[0] == '\n') ||
			len(peek) == 2 && peek[0] == '\r' && peek[1] == '\n' {
			return trim(line), nil
		}
	}

	mline := trim(line)

	// Read continuation lines.
	for s.skipSpace() {
		mline = append(mline, ' ')
		line := s.readLine()
		mline = append(mline, trim(line)...)
	}
	return mline, nil
}

// skipSpace skips one or multiple spaces and tabs in b.
func (s *headerScanner) skipSpace() bool {
	skipped := false
	for {
		c := s.b[s.r]
		if c != ' ' && c != '\t' {
			break
		}
		s.r++
		skipped = true
	}
	return skipped
}

func isASCIILetter(b byte) bool {
	b |= 0x20 // Make lower case.
	return 'a' <= b && b <= 'z'
}

// trim returns s with leading and trailing spaces and tabs removed.
// It does not assume Unicode or UTF-8.
func trim(s []byte) []byte {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	n := len(s)
	for n > i && (s[n-1] == ' ' || s[n-1] == '\t') {
		n--
	}
	return s[i:n]
}
