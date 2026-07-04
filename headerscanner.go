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

	// blockEnd is the end of the header block in b when the caller has
	// already found it (see readRawHeaders), 0 otherwise. next only trusts
	// it if the block really ends in CRLFCRLF there.
	blockEnd int

	key   []byte
	value []byte

	// keyHasSpace reports whether key contains a space that survives
	// trailing-whitespace trimming; such keys must not be canonicalized.
	keyHasSpace bool

	err error
}

func (s *headerScanner) next() bool {
	if !s.initialized {
		if bytes.HasPrefix(s.b, strCRLF) {
			s.r = 2
			return false
		}

		if s.blockEnd >= 4 && s.blockEnd <= len(s.b) &&
			bytes.Equal(s.b[s.blockEnd-4:s.blockEnd], strCRLFCRLF) {
			// The caller already found the end of the block, no need to
			// search for it again. The first CRLFCRLF can only sit at
			// blockEnd-4 since readRawHeaders stops at the first blank line.
			s.b = s.b[:s.blockEnd]
		} else {
			i := bytes.Index(s.b, strCRLFCRLF)
			if i < 0 {
				s.err = ErrNeedMore
				return false
			}
			s.b = s.b[:i+4]
		}
		if len(s.b) > 0 && (s.b[0] == ' ' || s.b[0] == '\t') {
			s.err = errors.New("invalid headers, headers cannot start with space or tab")
			return false
		}

		s.initialized = true
	}

	kv, colon, err := s.readContinuedLineSlice()
	if len(kv) == 0 {
		s.err = err
		return false
	}

	// Key ends at the first colon, already found by readContinuedLineSlice.
	k, v := kv[:colon], kv[colon+1:]
	valid, innerSpace := isValidHeaderKey(k)
	if !valid {
		s.err = fmt.Errorf("malformed MIME header line: %q", kv)
		return false
	}
	s.keyHasSpace = innerSpace

	// Skip initial spaces in value, without bytes.TrimLeft: it would
	// rebuild its ASCII set on every call.
	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t') {
		v = v[1:]
	}

	s.key = k
	s.value = v

	if err != nil {
		s.err = err
		return false
	}

	return true
}

// readLine reads a line from b, starting at s.r, and returns it with the
// trailing \n and a possible preceding \r dropped. b is truncated at the
// header block terminator, so every line ends in \n.
func (s *headerScanner) readLine() []byte {
	i := bytes.IndexByte(s.b[s.r:], '\n')
	if i < 0 {
		return nil
	}
	line := s.b[s.r : s.r+i]
	s.r += i + 1
	if i > 0 && line[i-1] == '\r' {
		line = line[:i-1]
	}
	return line
}

// readContinuedLineSlice reads continued lines from b until it finds a line
// that does not start with a space or tab, or it reaches the end of b.
// It also returns the position of the first colon in the returned line:
// the line can never start with a space or tab (the scanner rejects that for
// the first line and joins such lines into the previous header), so trimming
// it doesn't shift the colon.
func (s *headerScanner) readContinuedLineSlice() ([]byte, int, error) {
	line := s.readLine()
	if len(line) == 0 { // blank line - no continuation
		return line, -1, nil
	}

	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return nil, -1, fmt.Errorf("malformed MIME header: missing colon: %q", line)
	}

	// If the next line doesn't start with a space or tab, we are done.
	if len(s.b)-s.r > 1 {
		peek := s.b[s.r : s.r+2]
		if len(peek) > 0 && (isASCIILetter(peek[0]) || peek[0] == '\n') ||
			len(peek) == 2 && peek[0] == '\r' && peek[1] == '\n' {
			return trim(line), colon, nil
		}
	}

	mline := trim(line)

	// Read continuation lines.
	for s.skipSpace() {
		mline = append(mline, ' ')
		line := s.readLine()
		mline = append(mline, trim(line)...)
	}
	return mline, colon, nil
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

func trimTrailingSpace(s []byte) []byte {
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
