//go:build !amd64 && !arm64 && !ppc64 && !ppc64le && !riscv64 && !s390x

package fasthttp

const (
	maxHexIntChars = 7
)
