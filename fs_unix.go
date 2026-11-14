//go:build !windows

package fasthttp

var serveFilePathRewrite PathRewriteFunc

func hasWindowsReservedPathColon(_ []byte, _ bool) bool {
	return false
}
