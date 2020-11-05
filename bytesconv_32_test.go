// +build !amd64,!arm64,!ppc64,!ppc64le

package fasthttp

import (
	"testing"
)

func TestWriteHexInt(t *testing.T) {
	t.Parallel()

	testWriteHexInt(t, 0, "0")
	testWriteHexInt(t, 1, "1")
	testWriteHexInt(t, 0x123, "123")
	testWriteHexInt(t, 0x7fffffff, "7fffffff")
}

func TestAppendUint(t *testing.T) {
	t.Parallel()

	testAppendUint(t, 0)
	testAppendUint(t, 123)
	testAppendUint(t, 0x7fffffff)

	for i := 0; i < 2345; i++ {
		testAppendUint(t, i)
	}
}

func TestReadHexIntSuccess(t *testing.T) {
	t.Parallel()

	testReadHexIntSuccess(t, "0", 0)
	testReadHexIntSuccess(t, "fF", 0xff)
	testReadHexIntSuccess(t, "00abc", 0xabc)
	testReadHexIntSuccess(t, "7ffffff", 0x7ffffff)
	testReadHexIntSuccess(t, "000", 0)
	testReadHexIntSuccess(t, "1234ZZZ", 0x1234)
}

func TestParseUintError32(t *testing.T) {
	t.Parallel()

	// Overflow by last digit: 2 ** 32 / 2 * 10 ** n
	testParseUintError(t, "2147483648")
	testParseUintError(t, "21474836480")
	testParseUintError(t, "214748364800")
}

func TestParseUintSuccess(t *testing.T) {
	t.Parallel()

	testParseUintSuccess(t, "0", 0)
	testParseUintSuccess(t, "123", 123)
	testParseUintSuccess(t, "123456789", 123456789)

	// Max supported value: 2 ** 32 / 2 - 1
	testParseUintSuccess(t, "2147483647", 2147483647)
}
