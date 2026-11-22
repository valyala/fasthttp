//go:build !windows

package fasthttp

var serveFilePathRewrite PathRewriteFunc

func hasWindowsReservedPath(_ []byte, _ bool) bool {
	return false
}
