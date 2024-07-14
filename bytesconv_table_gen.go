//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
)

const (
	toLower = 'a' - 'A'
)

func main() {
	hex2intTable := func() [256]byte {
		var b [256]byte
		for i := 0; i < 256; i++ {
			c := byte(16)
			if i >= '0' && i <= '9' {
				c = byte(i) - '0'
			} else if i >= 'a' && i <= 'f' {
				c = byte(i) - 'a' + 10
			} else if i >= 'A' && i <= 'F' {
				c = byte(i) - 'A' + 10
			}
			b[i] = c
		}
		return b
	}()

	toLowerTable := func() [256]byte {
		var a [256]byte
		for i := 0; i < 256; i++ {
			c := byte(i)
			if c >= 'A' && c <= 'Z' {
				c += toLower
			}
			a[i] = c
		}
		return a
	}()

	toUpperTable := func() [256]byte {
		var a [256]byte
		for i := 0; i < 256; i++ {
			c := byte(i)
			if c >= 'a' && c <= 'z' {
				c -= toLower
			}
			a[i] = c
		}
		return a
	}()

	quotedArgShouldEscapeTable := func() [256]byte {
		// According to RFC 3986 §2.3
		var a [256]byte
		for i := 0; i < 256; i++ {
			a[i] = 1
		}

		// ALPHA
		for i := int('a'); i <= int('z'); i++ {
			a[i] = 0
		}
		for i := int('A'); i <= int('Z'); i++ {
			a[i] = 0
		}

		// DIGIT
		for i := int('0'); i <= int('9'); i++ {
			a[i] = 0
		}

		// Unreserved characters
		for _, v := range `-_.~` {
			a[v] = 0
		}

		return a
	}()

	quotedPathShouldEscapeTable := func() [256]byte {
		// The implementation here equal to net/url shouldEscape(s, encodePath)
		//
		// The RFC allows : @ & = + $ but saves / ; , for assigning
		// meaning to individual path segments. This package
		// only manipulates the path as a whole, so we allow those
		// last three as well. That leaves only ? to escape.
		a := quotedArgShouldEscapeTable

		for _, v := range `$&+,/:;=@` {
			a[v] = 0
		}

		return a
	}()

	validHeaderFieldByteTable := func() [128]byte {
		// Should match net/textproto's validHeaderFieldByte(c byte) bool
		// Defined by RFC 7230 and 9110:
		//
		//	header-field   = field-name ":" OWS field-value OWS
		//	field-name     = token
		//	tchar = "!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." /
		//	        "^" / "_" / "`" / "|" / "~" / DIGIT / ALPHA
		//	token = 1*tchar
		var table [128]byte
		for c := 0; c < 128; c++ {
			if (c >= '0' && c <= '9') ||
				(c >= 'a' && c <= 'z') ||
				(c >= 'A' && c <= 'Z') ||
				c == '!' || c == '#' || c == '$' || c == '%' || c == '&' ||
				c == '\'' || c == '*' || c == '+' || c == '-' || c == '.' ||
				c == '^' || c == '_' || c == '`' || c == '|' || c == '~' {
				table[c] = 1
			}
		}
		return table
	}()

	validHeaderValueByteTable := func() [256]byte {
		// Should match net/textproto's validHeaderValueByte(c byte) bool
		// Defined by RFC 7230 and 9110:
		//
		//	field-content  = field-vchar [ 1*( SP / HTAB ) field-vchar ]
		//	field-vchar    = VCHAR / obs-text
		//	obs-text       = %x80-FF
		//
		// RFC 5234:
		//
		//	HTAB           =  %x09
		//	SP             =  %x20
		//	VCHAR          =  %x21-7E
		var table [256]byte
		for c := 0; c < 256; c++ {
			if (c >= 0x21 && c <= 0x7E) || // VCHAR
				c == 0x20 || // SP
				c == 0x09 || // HTAB
				c >= 0x80 { // obs-text
				table[c] = 1
			}
		}
		return table
	}()

	validMethodValueByteTable := [256]byte{
		/*
				Same as net/http

			     Method         = "OPTIONS"                ; Section 9.2
			                    | "GET"                    ; Section 9.3
			                    | "HEAD"                   ; Section 9.4
			                    | "POST"                   ; Section 9.5
			                    | "PUT"                    ; Section 9.6
			                    | "DELETE"                 ; Section 9.7
			                    | "TRACE"                  ; Section 9.8
			                    | "CONNECT"                ; Section 9.9
			                    | extension-method
			   extension-method = token
			     token          = 1*<any CHAR except CTLs or separators>
		*/
		'!':  1,
		'#':  1,
		'$':  1,
		'%':  1,
		'&':  1,
		'\'': 1,
		'*':  1,
		'+':  1,
		'-':  1,
		'.':  1,
		'0':  1,
		'1':  1,
		'2':  1,
		'3':  1,
		'4':  1,
		'5':  1,
		'6':  1,
		'7':  1,
		'8':  1,
		'9':  1,
		'A':  1,
		'B':  1,
		'C':  1,
		'D':  1,
		'E':  1,
		'F':  1,
		'G':  1,
		'H':  1,
		'I':  1,
		'J':  1,
		'K':  1,
		'L':  1,
		'M':  1,
		'N':  1,
		'O':  1,
		'P':  1,
		'Q':  1,
		'R':  1,
		'S':  1,
		'T':  1,
		'U':  1,
		'W':  1,
		'V':  1,
		'X':  1,
		'Y':  1,
		'Z':  1,
		'^':  1,
		'_':  1,
		'`':  1,
		'a':  1,
		'b':  1,
		'c':  1,
		'd':  1,
		'e':  1,
		'f':  1,
		'g':  1,
		'h':  1,
		'i':  1,
		'j':  1,
		'k':  1,
		'l':  1,
		'm':  1,
		'n':  1,
		'o':  1,
		'p':  1,
		'q':  1,
		'r':  1,
		's':  1,
		't':  1,
		'u':  1,
		'v':  1,
		'w':  1,
		'x':  1,
		'y':  1,
		'z':  1,
		'|':  1,
		'~':  1,
	}

	w := bytes.NewBufferString(pre)
	fmt.Fprintf(w, "const hex2intTable = %q\n", hex2intTable)
	fmt.Fprintf(w, "const toLowerTable = %q\n", toLowerTable)
	fmt.Fprintf(w, "const toUpperTable = %q\n", toUpperTable)
	fmt.Fprintf(w, "const quotedArgShouldEscapeTable = %q\n", quotedArgShouldEscapeTable)
	fmt.Fprintf(w, "const quotedPathShouldEscapeTable = %q\n", quotedPathShouldEscapeTable)
	fmt.Fprintf(w, "const validHeaderFieldByteTable = %q\n", validHeaderFieldByteTable)
	fmt.Fprintf(w, "const validHeaderValueByteTable = %q\n", validHeaderValueByteTable)
	fmt.Fprintf(w, "const validMethodValueByteTable = %q\n", validMethodValueByteTable)

	if err := os.WriteFile("bytesconv_table.go", w.Bytes(), 0o660); err != nil {
		log.Fatal(err)
	}
}

const pre = `package fasthttp

// Code generated by go run bytesconv_table_gen.go; DO NOT EDIT.
// See bytesconv_table_gen.go for more information about these tables.

`
