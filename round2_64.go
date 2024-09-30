//go:build amd64 || arm64 || ppc64 || ppc64le || riscv64 || s390x

package fasthttp

func roundUpForSliceCap(n int) int {
	if n <= 0 {
		return 0
	}

	// Above 100MB, we don't round up as the overhead is too large.
	if n > 100*1024*1024 {
		return n
	}

	x := uint64(n - 1) // #nosec G115
	x |= x >> 1
	x |= x >> 2
	x |= x >> 4
	x |= x >> 8
	x |= x >> 16

	return int(x + 1) // #nosec G115
}
