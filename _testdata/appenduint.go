package main

var (
	maxIntChars = func() int {
		switch ^uint(0) {
		case 0xffffffff:
			// 32 bit
			return 9
		case 0xffffffffffffffff:
			// 64 bit
			return 18
		default:
			panic("Unsupported architecture :)")
		}
	}()
)

func AppendUint1(n int) []byte {
	if n < 0 {
		panic("BUG: int must be positive")
	}

	buf := make([]byte, maxIntChars+8)
	i := len(buf) - 1
	for {
		buf[i] = '0' + byte(n%10)
		n /= 10
		if n == 0 {
			break
		}
		i--
	}

	return buf[i:]
}

func AppendUint2(x int) []byte {
	if x < 0 {
		panic("PANIC!")
	}

	u := uint64(x)

	var a [64]byte
	i := 64

	if ^uintptr(0)>>32 == 0 {
		for u > uint64(^uintptr(0)) {
			q := u / 1e9
			us := uintptr(u - q*1e9) // us % 1e9 fits into a uintptr
			for j := 9; j > 0; j-- {
				i--
				qs := us / 10
				a[i] = byte(us - qs*10 + '0')
				us = qs
			}
			u = q
		}
	}

	// u guaranteed to fit into a uintptr
	us := uintptr(u)
	for us >= 10 {
		i--
		q := us / 10
		a[i] = byte(us - q*10 + '0')
		us = q
	}
	// u < 10
	i--
	a[i] = byte(us + '0')

	return a[i:]
}

var _b []byte
var i1, i2 = 1, 2

func main() {
	_b = AppendUint1(i1)
	_b = AppendUint2(i2)
}
