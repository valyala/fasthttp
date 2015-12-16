package fasthttp

import (
	"fmt"
	"reflect"
	"testing"
)

func TestUserData(t *testing.T) {
	var u userData

	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("key_%d", i))
		u.SetBytes(key, i+5)
		testUserDataGet(t, &u, key, i+5)
		u.SetBytes(key, i)
		testUserDataGet(t, &u, key, i)
	}

	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("key_%d", i))
		testUserDataGet(t, &u, key, i)
	}

	u.Reset()

	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("key_%d", i))
		testUserDataGet(t, &u, key, nil)
	}
}

func testUserDataGet(t *testing.T, u *userData, key []byte, value interface{}) {
	v := u.GetBytes(key)
	if v == nil && value != nil {
		t.Fatalf("cannot obtain value for key=%q", key)
	}
	if !reflect.DeepEqual(v, value) {
		t.Fatalf("unexpected value for key=%q: %d. Expecting %d", key, v, value)
	}
}
