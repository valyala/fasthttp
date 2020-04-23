// +build amd64 arm64 ppc64

package fasthttp

import (
	"testing"
)

func TestWriteHexInt(t *testing.T) {
	t.Parallel()

	testWriteHexInt(t, 0, "0")
	testWriteHexInt(t, 1, "1")
	testWriteHexInt(t, 0x123, "123")
	testWriteHexInt(t, 0x7fffffffffffffff, "7fffffffffffffff")
}

func TestAppendUint(t *testing.T) {
	t.Parallel()

	testAppendUint(t, 0)
	testAppendUint(t, 123)
	testAppendUint(t, 0x7fffffffffffffff)

	for i := 0; i < 2345; i++ {
		testAppendUint(t, i)
	}
}

func TestReadHexIntSuccess(t *testing.T) {
	t.Parallel()

	testReadHexIntSuccess(t, "0", 0)
	testReadHexIntSuccess(t, "fF", 0xff)
	testReadHexIntSuccess(t, "00abc", 0xabc)
	testReadHexIntSuccess(t, "7fffffff", 0x7fffffff)
	testReadHexIntSuccess(t, "000", 0)
	testReadHexIntSuccess(t, "1234ZZZ", 0x1234)
	testReadHexIntSuccess(t, "7ffffffffffffff", 0x7ffffffffffffff)
}

func TestParseUintError64(t *testing.T) {
	t.Parallel()

	// Overflow by last digit: 2 ** 64 / 2 * 10 ** n
	testParseUintError(t, "9223372036854775808")
	testParseUintError(t, "92233720368547758080")
	testParseUintError(t, "922337203685477580800")
}

func TestParseUintSuccess(t *testing.T) {
	t.Parallel()

	testParseUintSuccess(t, "0", 0)
	testParseUintSuccess(t, "123", 123)
	testParseUintSuccess(t, "1234567890", 1234567890)
	testParseUintSuccess(t, "123456789012345678", 123456789012345678)

	// Max supported value: 2 ** 64 / 2 - 1
	testParseUintSuccess(t, "9223372036854775807", 9223372036854775807)
}
