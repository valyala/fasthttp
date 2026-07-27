//go:build !windows

package fasthttp

func hasWindowsReservedPathColon(_ []byte, _ bool) bool {
	return false
}
