// +build amd64

package fasthttp

import (
	"testing"
)

func TestReadHexIntSuccess(t *testing.T) {
	testReadHexIntSuccess(t, "0", 0)
	testReadHexIntSuccess(t, "fF", 0xff)
	testReadHexIntSuccess(t, "00abc", 0xabc)
	testReadHexIntSuccess(t, "7fffffff", 0x7fffffff)
	testReadHexIntSuccess(t, "000", 0)
	testReadHexIntSuccess(t, "1234ZZZ", 0x1234)
	testReadHexIntSuccess(t, "7ffffffffffffff", 0x7ffffffffffffff)
}

func TestParseUintSuccess(t *testing.T) {
	testParseUintSuccess(t, "0", 0)
	testParseUintSuccess(t, "123", 123)
	testParseUintSuccess(t, "1234567890", 1234567890)
	testParseUintSuccess(t, "123456789012345678", 123456789012345678)
}
