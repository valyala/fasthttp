package prefork

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"reflect"
	"runtime"
	"testing"

	"github.com/valyala/fasthttp"
)

func setUp() {
	os.Args = append(os.Args, preforkChildFlag)
}

func tearDown() {
	os.Args = os.Args[:len(os.Args)-1]
}

func getAddr() string {
	return fmt.Sprintf("0.0.0.0:%d", rand.Intn(9000-3000)+3000)
}

func Test_IsChild(t *testing.T) {
	// This test can't run parallel as it modifies os.Args.

	v := IsChild()
	if v {
		t.Errorf("IsChild() == %v, want %v", v, false)
	}

	setUp()
	defer tearDown()

	v = IsChild()
	if !v {
		t.Errorf("IsChild() == %v, want %v", v, true)
	}
}

func Test_New(t *testing.T) {
	t.Parallel()

	s := &fasthttp.Server{}
	p := New(s)

	if p.Network != defaultNetwork {
		t.Errorf("Prefork.Netork == %s, want %s", p.Network, defaultNetwork)
	}

	if reflect.ValueOf(p.ServeFunc).Pointer() != reflect.ValueOf(s.Serve).Pointer() {
		t.Errorf("Prefork.ServeFunc == %p, want %p", p.ServeFunc, s.Serve)
	}

	if reflect.ValueOf(p.ServeTLSFunc).Pointer() != reflect.ValueOf(s.ServeTLS).Pointer() {
		t.Errorf("Prefork.ServeTLSFunc == %p, want %p", p.ServeTLSFunc, s.ServeTLS)
	}

	if reflect.ValueOf(p.ServeTLSEmbedFunc).Pointer() != reflect.ValueOf(s.ServeTLSEmbed).Pointer() {
		t.Errorf("Prefork.ServeTLSFunc == %p, want %p", p.ServeTLSEmbedFunc, s.ServeTLSEmbed)
	}
}

func Test_listen(t *testing.T) {
	t.Parallel()

	p := &Prefork{
		Reuseport: true,
	}
	addr := getAddr()

	ln, err := p.listen(addr)

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	ln.Close()

	lnAddr := ln.Addr().String()
	if lnAddr != addr {
		t.Errorf("Prefork.Addr == %s, want %s", lnAddr, addr)
	}

	if p.Network != defaultNetwork {
		t.Errorf("Prefork.Network == %s, want %s", p.Network, defaultNetwork)
	}

	procs := runtime.GOMAXPROCS(0)
	if procs != 1 {
		t.Errorf("GOMAXPROCS == %d, want %d", procs, 1)
	}
}

func Test_setTCPListenerFiles(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	p := &Prefork{}
	addr := getAddr()

	err := p.setTCPListenerFiles(addr)

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if p.ln == nil {
		t.Fatal("Prefork.ln is nil")
	}

	p.ln.Close()

	lnAddr := p.ln.Addr().String()
	if lnAddr != addr {
		t.Errorf("Prefork.Addr == %s, want %s", lnAddr, addr)
	}

	if p.Network != defaultNetwork {
		t.Errorf("Prefork.Network == %s, want %s", p.Network, defaultNetwork)
	}

	if len(p.files) != 1 {
		t.Errorf("Prefork.files == %d, want %d", len(p.files), 1)
	}
}

func Test_ListenAndServe(t *testing.T) {
	// This test can't run parallel as it modifies os.Args.

	setUp()
	defer tearDown()

	s := &fasthttp.Server{}
	p := New(s)
	p.Reuseport = true
	p.ServeFunc = func(ln net.Listener) error {
		return nil
	}

	addr := getAddr()

	err := p.ListenAndServe(addr)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	p.ln.Close()

	lnAddr := p.ln.Addr().String()
	if lnAddr != addr {
		t.Errorf("Prefork.Addr == %s, want %s", lnAddr, addr)
	}

	if p.ln == nil {
		t.Error("Prefork.ln is nil")
	}
}

func Test_ListenAndServeTLS(t *testing.T) {
	// This test can't run parallel as it modifies os.Args.

	setUp()
	defer tearDown()

	s := &fasthttp.Server{}
	p := New(s)
	p.Reuseport = true
	p.ServeTLSFunc = func(ln net.Listener, certFile, keyFile string) error {
		return nil
	}

	addr := getAddr()

	err := p.ListenAndServeTLS(addr, "./key", "./cert")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	p.ln.Close()

	lnAddr := p.ln.Addr().String()
	if lnAddr != addr {
		t.Errorf("Prefork.Addr == %s, want %s", lnAddr, addr)
	}

	if p.ln == nil {
		t.Error("Prefork.ln is nil")
	}
}

func Test_ListenAndServeTLSEmbed(t *testing.T) {
	// This test can't run parallel as it modifies os.Args.

	setUp()
	defer tearDown()

	s := &fasthttp.Server{}
	p := New(s)
	p.Reuseport = true
	p.ServeTLSEmbedFunc = func(ln net.Listener, certData, keyData []byte) error {
		return nil
	}

	addr := getAddr()

	err := p.ListenAndServeTLSEmbed(addr, []byte("key"), []byte("cert"))
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	p.ln.Close()

	lnAddr := p.ln.Addr().String()
	if lnAddr != addr {
		t.Errorf("Prefork.Addr == %s, want %s", lnAddr, addr)
	}

	if p.ln == nil {
		t.Error("Prefork.ln is nil")
	}
}
