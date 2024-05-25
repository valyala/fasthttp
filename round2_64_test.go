//go:build amd64 || arm64 || ppc64 || ppc64le || riscv64 || s390x

package fasthttp

import (
	"math"
	"testing"
)

func TestRound2ForSliceCap(t *testing.T) {
	t.Parallel()

	testRound2ForSliceCap(t, 0, 0)
	testRound2ForSliceCap(t, 1, 1)
	testRound2ForSliceCap(t, 2, 2)
	testRound2ForSliceCap(t, 3, 4)
	testRound2ForSliceCap(t, 4, 4)
	testRound2ForSliceCap(t, 5, 8)
	testRound2ForSliceCap(t, 7, 8)
	testRound2ForSliceCap(t, 8, 8)
	testRound2ForSliceCap(t, 9, 16)
	testRound2ForSliceCap(t, 0x10001, 0x20000)

	testRound2ForSliceCap(t, math.MaxInt32, math.MaxInt32)
	testRound2ForSliceCap(t, math.MaxInt64-1, math.MaxInt64-1)
}

func testRound2ForSliceCap(t *testing.T, n, expectedRound2 int) {
	if roundUpForSliceCap(n) != expectedRound2 {
		t.Fatalf("Unexpected round2(%d)=%d. Expected %d", n, roundUpForSliceCap(n), expectedRound2)
	}
}
