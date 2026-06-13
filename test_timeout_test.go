package fasthttp

import (
	"runtime"
	"time"
)

func testTimeout(timeout time.Duration) time.Duration {
	if raceEnabled {
		timeout *= 5
	}
	if runtime.GOOS == "windows" {
		timeout *= 2
	}
	return timeout
}
