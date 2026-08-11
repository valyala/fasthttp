package fasthttp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

type headerScanner struct {
	initialized bool

	b []byte
	r int

	key   []byte
	value []byte

	// keyHasSpace reports whether key contains a space that survives
	// trailing-whitespace trimming; such keys must not be canonicalized.
	keyHasSpace bool

	// valueValid reports whether value holds only valid header value bytes;
	// the scan that finds the line end checks them on the way.
	valueValid bool

	// raw, when set, receives the wire bytes of b up to r before a fold
	// overwrites them; rawSaved is the secured watermark. Folding joins
	// continuation lines in place, which is the one operation that rewrites
	// b, so everything before rawSaved plus everything after it that a fold
	// never touched reassembles the exact wire bytes.
	raw      *[]byte
	rawSaved int

	// blockEnd memoizes the end of the header block; b does not change
	// during a scan, so the terminator is searched for at most once.
	blockEnd int

	err error
}

func (s *headerScanner) next() bool {
	if !s.initialized {
		if bytes.HasPrefix(s.b, strCRLF) {
			s.r = 2
			return false
		}
		if len(s.b) > 0 && (s.b[0] == ' ' || s.b[0] == '\t') {
			s.err = errors.New("invalid headers, headers cannot start with space or tab")
			return false
		}
		s.initialized = true
	}

	// One pass finds the colon, validates the key and tracks folding-relevant
	// spaces; anything unusual falls back to the line-oriented reader.
	b := s.b
	r := s.r
	if r >= len(b) {
		s.err = ErrNeedMore
		return false
	}
	switch b[r] {
	case '\r':
		if r+1 >= len(b) {
			s.err = ErrNeedMore
			return false
		}
		if b[r+1] == '\n' {
			// The block still has to carry a CRLFCRLF, or a peer that only
			// accepts that terminator would frame the stream differently.
			// The line before this one ending in CRLF puts one right here.
			if (r >= 2 && b[r-1] == '\n' && b[r-2] == '\r') || s.blockComplete() {
				s.r = r + 2
				return false
			}
			s.err = ErrNeedMore
			return false
		}
	case '\n':
		// A bare-LF blank line ends the block, but only when the block is
		// really terminated by CRLFCRLF; otherwise more data may still turn
		// it into one.
		if s.blockComplete() {
			s.r = r + 1
			return false
		}
		s.err = ErrNeedMore
		return false
	}
	i := r
	seenSpace := false
	innerSpace := false
	for {
		if i >= len(b) {
			s.err = ErrNeedMore
			return false
		}
		c := b[i]
		if c == ':' {
			break
		}
		if validHeaderFieldByte(c) {
			if seenSpace {
				innerSpace = true
			}
			i++
			continue
		}
		if c == ' ' {
			seenSpace = true
			i++
			continue
		}
		return s.failLine(r)
	}
	if i == r {
		return s.failLine(r)
	}
	colon := i
	i++
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	valStart := i
	j, valueValid := scanValueLine(b[i:])
	if j < 0 {
		s.err = ErrNeedMore
		return false
	}
	lineEnd := i + j
	valEnd := lineEnd
	if valEnd > valStart && b[valEnd-1] == '\r' {
		valEnd--
	}
	for valEnd > valStart && (b[valEnd-1] == ' ' || b[valEnd-1] == '\t') {
		valEnd--
	}
	nr := lineEnd + 1
	if nr >= len(b) {
		s.err = ErrNeedMore
		return false
	}
	if c := b[nr]; c == ' ' || c == '\t' {
		return s.nextFolded()
	}
	s.r = nr
	s.key = b[r:colon]
	s.value = b[valStart:valEnd]
	s.keyHasSpace = innerSpace
	s.valueValid = valueValid
	return true
}

// blockComplete reports whether the header block terminator is present.
// The search runs at most once per scan: b is fixed for its duration, and a
// fold only ever rewrites bytes it has already consumed.
func (s *headerScanner) blockComplete() bool {
	if s.blockEnd != 0 {
		return true
	}
	i := bytes.Index(s.b, strCRLFCRLF)
	if i < 0 {
		return false
	}
	s.blockEnd = i + len(strCRLFCRLF)
	return true
}

// failLine reports a bad header line with the same errors the line reader
// would produce, waiting for the full line first.
func (s *headerScanner) failLine(r int) bool {
	end := bytes.IndexByte(s.b[r:], '\n')
	if end < 0 {
		s.err = ErrNeedMore
		return false
	}
	line := s.b[r : r+end]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if bytes.IndexByte(line, ':') < 0 {
		s.err = fmt.Errorf("malformed mime header: missing colon: %q", line)
		return false
	}
	s.err = fmt.Errorf("malformed mime header line: %q", trim(line))
	return false
}

// nextFolded handles a header with continuation lines.
func (s *headerScanner) nextFolded() bool {
	kv, colon, err := s.readContinuedLineSlice()
	if len(kv) == 0 {
		s.err = err
		return false
	}

	k, v := kv[:colon], kv[colon+1:]
	valid, innerSpace := isValidHeaderKey(k)
	if !valid {
		s.err = fmt.Errorf("malformed mime header line: %q", kv)
		return false
	}
	s.keyHasSpace = innerSpace

	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t') {
		v = v[1:]
	}

	s.key = k
	s.value = v
	s.valueValid = validHeaderValueBytes(v)

	return true
}

// readLine returns the next line without its \n and a possible preceding \r.
// ok is false when b holds no complete line yet. crlf reports a \r\n ending,
// which is what makes an empty line a valid block terminator.
func (s *headerScanner) readLine() (line []byte, crlf, ok bool) {
	i := bytes.IndexByte(s.b[s.r:], '\n')
	if i < 0 {
		return nil, false, false
	}
	line = s.b[s.r : s.r+i]
	s.r += i + 1
	if i > 0 && line[i-1] == '\r' {
		return line[:i-1], true, true
	}
	return line, false, true
}

// readContinuedLineSlice reads the next header line, joining continuation
// lines into joined. It also returns the position of the first colon: the
// line can never start with a space or tab (the scanner rejects that for the
// first line and joins such lines into the previous header), so trimming it
// doesn't shift the colon. A nil line with a nil error is the block end.
func (s *headerScanner) readContinuedLineSlice() ([]byte, int, error) {
	line, crlf, ok := s.readLine()
	if !ok {
		return nil, -1, ErrNeedMore
	}
	if len(line) == 0 {
		if !crlf && !s.blockComplete() {
			return nil, -1, ErrNeedMore
		}
		return nil, -1, nil
	}

	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return nil, -1, fmt.Errorf("malformed mime header: missing colon: %q", line)
	}

	// The next byte decides whether a continuation follows; without it the
	// header cannot be finalized yet.
	if s.r >= len(s.b) {
		return nil, -1, ErrNeedMore
	}
	if c := s.b[s.r]; c != ' ' && c != '\t' {
		return trim(line), colon, nil
	}

	// A fold rewrites b, so a retry with more data cannot reparse it; the
	// whole block must be present before joining starts.
	if !s.blockComplete() {
		return nil, -1, ErrNeedMore
	}

	// Join continuation lines in place; the joined value only ever grows
	// over bytes of its own already-consumed lines.
	s.secureRaw()
	mline := trim(line)
	for {
		skipped, err := s.skipSpace()
		if err != nil {
			return nil, -1, err
		}
		if !skipped {
			break
		}
		line, _, ok := s.readLine()
		if !ok {
			return nil, -1, ErrNeedMore
		}
		s.secureRaw()
		mline = append(mline, ' ')
		mline = append(mline, trim(line)...)
	}
	return mline, colon, nil
}

// secureRaw copies the not-yet-secured wire bytes below the read position
// into raw before a fold overwrites them.
func (s *headerScanner) secureRaw() {
	if s.raw == nil {
		return
	}
	if cap(*s.raw) < s.r {
		// Every later fold appends into this buffer too, so grow ahead of
		// the block instead of once per continuation. The bound follows the
		// bytes consumed so far, never the rest of the buffer.
		grown := make([]byte, len(*s.raw), 2*s.r)
		copy(grown, *s.raw)
		*s.raw = grown
	}
	*s.raw = append(*s.raw, s.b[s.rawSaved:s.r]...)
	s.rawSaved = s.r
}

// skipSpace skips one or multiple spaces and tabs in b.
func (s *headerScanner) skipSpace() (bool, error) {
	skipped := false
	for {
		if s.r >= len(s.b) {
			return false, ErrNeedMore
		}
		c := s.b[s.r]
		if c != ' ' && c != '\t' {
			break
		}
		s.r++
		skipped = true
	}
	return skipped, nil
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

// scanValueLine returns the index of the LF ending a header value line and
// reports whether every byte before it is a valid header value byte. Eight
// bytes at a time: any lane below 0x20 or equal to 0x7f drops that word to
// the byte-wise branch, which is where CR, LF and TAB are handled.
func scanValueLine(b []byte) (lf int, valid bool) {
	const ones = 0x0101010101010101
	const highs = 0x8080808080808080
	valid = true
	i := 0
	for ; i+8 <= len(b); i += 8 {
		x := binary.LittleEndian.Uint64(b[i:])
		bad := (x - ones*0x20) & ^x & highs
		y := x ^ (ones * 0x7f)
		bad |= (y - ones) & ^y & highs
		if bad == 0 {
			continue
		}
		for j := i; j < i+8; j++ {
			switch c := b[j]; {
			case c == '\n':
				return j, valid
			case c == '\r':
				if j+1 < len(b) && b[j+1] == '\n' {
					return j + 1, valid
				}
				valid = false
			case validHeaderValueByteTable[c] == 0:
				valid = false
			}
		}
	}
	for ; i < len(b); i++ {
		switch c := b[i]; {
		case c == '\n':
			return i, valid
		case c == '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				return i + 1, valid
			}
			valid = false
		case validHeaderValueByteTable[c] == 0:
			valid = false
		}
	}
	return -1, valid
}
