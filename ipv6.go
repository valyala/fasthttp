package fasthttp

import (
	"bytes"
	"errors"
)

var (
	errInvalidIPv6Host    = errors.New("invalid IPv6 host")
	errInvalidIPv6Zone    = errors.New("invalid IPv6 zone")
	errInvalidIPv6Address = errors.New("invalid IPv6 address")
)

func validateIPv6Literal(host []byte) error {
	if len(host) == 0 || host[0] != '[' {
		return nil
	}
	end := bytes.IndexByte(host, ']')
	if end < 0 || end == 1 {
		return errInvalidIPv6Host
	}
	addr := host[1:end]

	// Optional zone.
	if zi := bytes.IndexByte(addr, '%'); zi >= 0 {
		if zi == len(addr)-1 {
			return errInvalidIPv6Zone
		}
		addr = addr[:zi]
	}

	// Must have a colon to be IPv6.
	if bytes.IndexByte(addr, ':') < 0 {
		return errInvalidIPv6Address
	}

	// IPv4-embedded?
	if bytes.IndexByte(addr, '.') >= 0 {
		lastColon := bytes.LastIndexByte(addr, ':')
		if lastColon < 0 || lastColon == len(addr)-1 {
			return errInvalidIPv6Address
		}

		ipv4 := addr[lastColon+1:]
		if !validIPv4(ipv4) {
			return errInvalidIPv6Address
		}

		head := addr[:lastColon]
		seenDoubleAtSplit := lastColon > 0 && addr[lastColon-1] == ':'
		if seenDoubleAtSplit {
			head = addr[:lastColon-1]
		}

		hextets, seenDoubleHead, ok := parseIPv6Hextets(head, false)
		if !ok {
			return errInvalidIPv6Address
		}

		if seenDoubleHead && seenDoubleAtSplit {
			return errInvalidIPv6Address
		}

		hextets += 2 // IPv4 tail = 2 hextets
		seenDouble := seenDoubleHead || seenDoubleAtSplit

		// '::' must compress at least one hextet.
		if (!seenDouble && hextets != 8) || (seenDouble && hextets >= 8) {
			return errInvalidIPv6Address
		}
		return nil
	}

	// Pure IPv6
	hextets, seenDouble, ok := parseIPv6Hextets(addr, false)
	if !ok {
		return errInvalidIPv6Address
	}
	if (!seenDouble && hextets != 8) || (seenDouble && hextets >= 8) {
		return errInvalidIPv6Address
	}
	return nil
}

func parseIPv6Hextets(s []byte, allowTrailingColon bool) (groups int, seenDouble, ok bool) {
	n := len(s)
	if n == 0 {
		return 0, false, true
	}
	i := 0
	justSawDouble := false

	for i < n {
		if s[i] == ':' {
			if i+1 < n && s[i+1] == ':' {
				if seenDouble || justSawDouble {
					return 0, false, false
				}
				seenDouble = true
				justSawDouble = true
				i += 2
				if i == n {
					break
				}
				continue
			}
			if i == 0 {
				return 0, false, false
			}
			if justSawDouble {
				return 0, false, false
			}
			if i == n-1 {
				if allowTrailingColon {
					break
				}
				return 0, false, false
			}
			if !ishex(s[i+1]) {
				return 0, false, false
			}
			i++
			continue
		}

		justSawDouble = false
		cnt := 0
		for cnt < 4 && i < n && ishex(s[i]) {
			i++
			cnt++
		}
		if cnt == 0 {
			return 0, false, false
		}
		groups++

		if i < n && s[i] != ':' {
			return 0, false, false
		}
	}
	return groups, seenDouble, true
}

// validIPv4 validates a dotted-quad (exactly 4 parts, 0..255) with no leading zeros
// unless the octet is exactly "0".
func validIPv4(s []byte) bool {
	parts := 0
	i := 0
	n := len(s)

	for parts < 4 {
		if i >= n {
			return false
		}

		start := i
		val := 0
		digits := 0

		for i < n {
			c := s[i]
			if c < '0' || c > '9' {
				break
			}
			val = val*10 + int(c-'0')
			if val > 255 {
				return false
			}
			i++
			digits++
			if digits > 3 {
				return false
			}
		}
		if digits == 0 {
			return false
		}

		// Disallow leading zeros like "00", "01", "001".
		// Allowed: exactly "0" or any number that doesn't start with '0'.
		if digits > 1 && s[start] == '0' {
			return false
		}

		parts++
		if parts == 4 {
			return i == n // must consume all input
		}
		if i >= n || s[i] != '.' {
			return false
		}
		i++ // skip dot
	}
	return false
}
