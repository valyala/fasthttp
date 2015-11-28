// +build !amd64

package fasthttp

import (
	"testing"
)

func TestAppendUint(t *testing.T) {
	testAppendUint(t, 0)
	testAppendUint(t, 123)
	testAppendUint(t, 0x7fffffff)
}

func TestReadHexIntSuccess(t *testing.T) {
	testReadHexIntSuccess(t, "0", 0)
	testReadHexIntSuccess(t, "fF", 0xff)
	testReadHexIntSuccess(t, "00abc", 0xabc)
	testReadHexIntSuccess(t, "7ffffff", 0x7ffffff)
	testReadHexIntSuccess(t, "000", 0)
	testReadHexIntSuccess(t, "1234ZZZ", 0x1234)
}

func TestParseUintSuccess(t *testing.T) {
	testParseUintSuccess(t, "0", 0)
	testParseUintSuccess(t, "123", 123)
	testParseUintSuccess(t, "123456789", 123456789)
}
