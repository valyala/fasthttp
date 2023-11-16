package fasthttputil_test

import (
	"sync/atomic"
	"testing"

	"github.com/valyala/fasthttp/fasthttputil"
)

func TestOnceFunc(t *testing.T) {
	t.Parallel()

	var p int32

	increment := func() {
		_ = atomic.AddInt32(&p, 1)
	}

	onceIncrement := fasthttputil.OnceFunc(increment)

	onceIncrement()
	onceIncrement()
	onceIncrement()

	got := atomic.LoadInt32(&p)
	expect := int32(1)

	if got != expect {
		t.Fatalf("unexpected result: got %v, expect %v", got, expect)
	}
}
