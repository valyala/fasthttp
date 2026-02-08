//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || rumprun || (zos && s390x)

package listensocket

import (
	"errors"
	"math"
)

// SafeIntToUint32 converts int to uint32 and returns an error on overflow.
func SafeIntToUint32(i int) (uint32, error) {
	if i < 0 {
		return 0, errors.New("value is negative, cannot convert to uint32")
	}
	ui := uint64(i)
	if ui > math.MaxUint32 {
		return 0, errors.New("value exceeds uint32 max value")
	}
	return uint32(ui), nil
}
