package http2

import "errors"

// Urgency values (RFC 9218, Section 4.1).
const (
	defaultUrgency = 3
	maxUrgency     = 7
)

type priority struct {
	urgency     uint8
	incremental bool
}

var errPriorityFormat = errors.New("http2: malformed priority field")

// parsePriority parses an RFC 9218 Priority field value, an RFC 8941
// Dictionary. A syntax error anywhere fails the field; a member of the wrong
// type or out of range is ignored on its own, an unknown key too, and a
// repeated key takes its last value.
func parsePriority(value string) (priority, error) {
	parser := sfParser{s: value}
	var members priorityMembers
	if err := parser.dictionary(&members); err != nil {
		return priority{}, err
	}
	result := priority{urgency: defaultUrgency}
	if members.urgency.kind == sfInteger && members.urgency.integer >= 0 && members.urgency.integer <= maxUrgency {
		result.urgency = uint8(members.urgency.integer)
	}
	if members.incremental.kind == sfBoolean {
		result.incremental = members.incremental.boolean
	}
	return result, nil
}

// priorityMembers holds the last value of the two members RFC 9218 defines.
type priorityMembers struct {
	urgency     sfItem
	incremental sfItem
}

func (m *priorityMembers) set(key string, item sfItem) {
	switch key {
	case "u":
		m.urgency = item
	case "i":
		m.incremental = item
	}
}

type sfKind uint8

const (
	sfNone sfKind = iota
	sfInteger
	sfDecimal
	sfString
	sfToken
	sfBinary
	sfBoolean
	sfInnerList
)

// sfItem is a parsed member value: its kind, and the value of an Integer or
// Boolean, the only ones the priority semantics read.
type sfItem struct {
	kind    sfKind
	integer int64
	boolean bool
}

// sfParser parses RFC 8941 Structured Fields without allocating.
type sfParser struct {
	s   string
	pos int
}

func (p *sfParser) more() bool { return p.pos < len(p.s) }

func (p *sfParser) peek() byte { return p.s[p.pos] }

func (p *sfParser) skip(accept func(byte) bool) {
	for p.more() && accept(p.peek()) {
		p.pos++
	}
}

func isSP(b byte) bool { return b == ' ' }

func isOWS(b byte) bool { return b == ' ' || b == '\t' }

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isLCAlpha(b byte) bool { return b >= 'a' && b <= 'z' }

func isAlpha(b byte) bool { return isLCAlpha(b) || b >= 'A' && b <= 'Z' }

func isTokenChar(b byte) bool {
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~', ':', '/':
		return true
	}
	return isAlpha(b) || isDigit(b)
}

func isBase64Char(b byte) bool {
	return isAlpha(b) || isDigit(b) || b == '+' || b == '/'
}

// dictionary parses sf-dictionary (RFC 8941, Section 4.2.2), handing each
// member to members in order.
func (p *sfParser) dictionary(members *priorityMembers) error {
	p.skip(isSP)
	for p.more() {
		key, err := p.key()
		if err != nil {
			return err
		}
		item := sfItem{kind: sfBoolean, boolean: true}
		if p.more() && p.peek() == '=' {
			p.pos++
			if item, err = p.memberValue(); err != nil {
				return err
			}
		} else if err := p.parameters(); err != nil {
			return err
		}
		members.set(key, item)
		p.skip(isOWS)
		if !p.more() {
			break
		}
		if p.peek() != ',' {
			return errPriorityFormat
		}
		p.pos++
		p.skip(isOWS)
		if !p.more() {
			return errPriorityFormat
		}
	}
	return nil
}

func (p *sfParser) key() (string, error) {
	start := p.pos
	if !p.more() || !isLCAlpha(p.peek()) && p.peek() != '*' {
		return "", errPriorityFormat
	}
	p.pos++
	for p.more() {
		b := p.peek()
		if !isLCAlpha(b) && !isDigit(b) && b != '_' && b != '-' && b != '.' && b != '*' {
			break
		}
		p.pos++
	}
	return p.s[start:p.pos], nil
}

// memberValue parses sf-item or inner-list, parameters included.
func (p *sfParser) memberValue() (sfItem, error) {
	if p.more() && p.peek() == '(' {
		return p.innerList()
	}
	return p.item()
}

func (p *sfParser) item() (sfItem, error) {
	item, err := p.bareItem()
	if err != nil {
		return sfItem{}, err
	}
	return item, p.parameters()
}

func (p *sfParser) bareItem() (sfItem, error) {
	if !p.more() {
		return sfItem{}, errPriorityFormat
	}
	switch b := p.peek(); {
	case b == '-' || isDigit(b):
		return p.number()
	case b == '"':
		return p.quotedString()
	case isAlpha(b) || b == '*':
		return p.token()
	case b == ':':
		return p.binary()
	case b == '?':
		return p.boolean()
	}
	return sfItem{}, errPriorityFormat
}

// number parses sf-integer or sf-decimal (RFC 8941, Section 4.2.4).
func (p *sfParser) number() (sfItem, error) {
	negative := p.peek() == '-'
	if negative {
		p.pos++
	}
	var integer int64
	digits, fraction := 0, -1
	for p.more() {
		b := p.peek()
		switch {
		case isDigit(b):
			if fraction >= 0 {
				fraction++
				if fraction > 3 {
					return sfItem{}, errPriorityFormat
				}
			} else {
				digits++
				if digits > 15 {
					return sfItem{}, errPriorityFormat
				}
				integer = integer*10 + int64(b-'0')
			}
		case b == '.' && fraction < 0:
			if digits > 12 {
				return sfItem{}, errPriorityFormat
			}
			fraction = 0
		default:
			goto done
		}
		p.pos++
	}
done:
	if digits == 0 || fraction == 0 {
		return sfItem{}, errPriorityFormat
	}
	if fraction > 0 {
		return sfItem{kind: sfDecimal}, nil
	}
	if negative {
		integer = -integer
	}
	return sfItem{kind: sfInteger, integer: integer}, nil
}

// quotedString parses sf-string (RFC 8941, Section 4.2.5).
func (p *sfParser) quotedString() (sfItem, error) {
	p.pos++
	for p.more() {
		b := p.peek()
		p.pos++
		switch {
		case b == '\\':
			if !p.more() || p.peek() != '"' && p.peek() != '\\' {
				return sfItem{}, errPriorityFormat
			}
			p.pos++
		case b == '"':
			return sfItem{kind: sfString}, nil
		case b < 0x20 || b >= 0x7f:
			return sfItem{}, errPriorityFormat
		}
	}
	return sfItem{}, errPriorityFormat
}

// token parses sf-token (RFC 8941, Section 4.2.6); the caller checked its first
// byte.
func (p *sfParser) token() (sfItem, error) {
	p.pos++
	p.skip(isTokenChar)
	return sfItem{kind: sfToken}, nil
}

// binary parses sf-binary (RFC 8941, Section 4.2.7): base64 characters, padding
// only at the end, in a length that decodes once padding is synthesized
// (RFC 4648, Section 4): at most two '=', a padded length that is a multiple of
// four, and no lone sextet.
func (p *sfParser) binary() (sfItem, error) {
	p.pos++
	start := p.pos
	p.skip(isBase64Char)
	data := p.pos - start
	p.skip(func(b byte) bool { return b == '=' })
	pad := p.pos - start - data
	if !p.more() || p.peek() != ':' {
		return sfItem{}, errPriorityFormat
	}
	if pad > 2 || pad != 0 && (data+pad)%4 != 0 || data%4 == 1 {
		return sfItem{}, errPriorityFormat
	}
	p.pos++
	return sfItem{kind: sfBinary}, nil
}

// boolean parses sf-boolean (RFC 8941, Section 4.2.8).
func (p *sfParser) boolean() (sfItem, error) {
	p.pos++
	if !p.more() || p.peek() != '0' && p.peek() != '1' {
		return sfItem{}, errPriorityFormat
	}
	item := sfItem{kind: sfBoolean, boolean: p.peek() == '1'}
	p.pos++
	return item, nil
}

// parameters parses and drops parameters (RFC 8941, Section 4.2.3.2).
func (p *sfParser) parameters() error {
	for p.more() && p.peek() == ';' {
		p.pos++
		p.skip(isSP)
		if _, err := p.key(); err != nil {
			return err
		}
		if p.more() && p.peek() == '=' {
			p.pos++
			if _, err := p.bareItem(); err != nil {
				return err
			}
		}
	}
	return nil
}

// innerList parses inner-list (RFC 8941, Section 4.2.1.2), parameters included.
func (p *sfParser) innerList() (sfItem, error) {
	p.pos++
	for {
		p.skip(isSP)
		if !p.more() {
			return sfItem{}, errPriorityFormat
		}
		if p.peek() == ')' {
			p.pos++
			return sfItem{kind: sfInnerList}, p.parameters()
		}
		if _, err := p.item(); err != nil {
			return sfItem{}, err
		}
		if !p.more() || p.peek() != ' ' && p.peek() != ')' {
			return sfItem{}, errPriorityFormat
		}
	}
}
