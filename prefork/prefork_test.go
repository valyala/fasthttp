package prefork

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/valyala/fasthttp"
)

func setUp() {
	os.Setenv(preforkChildEnvVariable, "1")
}

func tearDown() {
	os.Unsetenv(preforkChildEnvVariable)
}

func getAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", rand.Intn(9000-3000)+3000)
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
		t.Errorf("Prefork.Network == %q, want %q", p.Network, defaultNetwork)
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
		t.Errorf("Prefork.Addr == %q, want %q", lnAddr, addr)
	}

	if p.Network != defaultNetwork {
		t.Errorf("Prefork.Network == %q, want %q", p.Network, defaultNetwork)
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
		t.Errorf("Prefork.Addr == %q, want %q", lnAddr, addr)
	}

	if p.Network != defaultNetwork {
		t.Errorf("Prefork.Network == %q, want %q", p.Network, defaultNetwork)
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
		t.Errorf("Prefork.Addr == %q, want %q", lnAddr, addr)
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
		t.Errorf("Prefork.Addr == %q, want %q", lnAddr, addr)
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
		t.Errorf("Prefork.Addr == %q, want %q", lnAddr, addr)
	}

	if p.ln == nil {
		t.Error("Prefork.ln is nil")
	}
}

func Test_Prefork_Logger(t *testing.T) {
	t.Parallel()

	s := &fasthttp.Server{}
	p := New(s)

	// Test default logger
	logger := p.logger()
	if logger == nil {
		t.Error("Default logger should not be nil")
	}

	// Test custom logger
	customLogger := &testLogger{}
	p.Logger = customLogger
	if p.logger() != customLogger {
		t.Error("Custom logger should be returned")
	}
}

type testLogger struct {
	messages []string
}

func (l *testLogger) Printf(format string, args ...any) {
	l.messages = append(l.messages, fmt.Sprintf(format, args...))
}

// Test_Prefork_Lifecycle runs the full prefork lifecycle with a CommandProducer
// and verifies that callbacks are invoked in the correct order with the correct arguments.
func Test_Prefork_Lifecycle(t *testing.T) {
	t.Parallel()

	type event struct {
		name string
		pids []int
	}

	var mu sync.Mutex
	var events []event
	record := func(name string, pids ...int) {
		mu.Lock()
		events = append(events, event{name, pids})
		mu.Unlock()
	}

	p := &Prefork{
		Reuseport:        true,
		RecoverThreshold: 1,
		Logger:           &testLogger{},
		CommandProducer: func(files []*os.File) (*exec.Cmd, error) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), preforkChildEnvVariable+"=1")
			err := cmd.Start()
			return cmd, err
		},
		OnChildSpawn: func(pid int) error {
			record("spawn", pid)
			return nil
		},
		OnMasterReady: func(childPIDs []int) error {
			record("ready", childPIDs...)
			return nil
		},
		OnChildRecover: func(oldPid, newPid int) {
			record("recover", oldPid, newPid)
		},
	}

	err := p.prefork(getAddr())
	if !errors.Is(err, ErrOverRecovery) {
		t.Fatalf("expected ErrOverRecovery, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Verify we got spawn events for initial children
	var spawnCount int
	var readyCount int
	var recoverCount int
	for _, e := range events {
		switch e.name {
		case "spawn":
			spawnCount++
			if len(e.pids) != 1 || e.pids[0] <= 0 {
				t.Errorf("spawn event has invalid PID: %v", e.pids)
			}
		case "ready":
			readyCount++
			if len(e.pids) == 0 {
				t.Error("ready event received empty PID list")
			}
		case "recover":
			recoverCount++
			if len(e.pids) != 2 || e.pids[0] <= 0 || e.pids[1] <= 0 {
				t.Errorf("recover event has invalid PIDs: %v", e.pids)
			}
			if e.pids[0] == e.pids[1] {
				t.Error("recover old and new PID should differ")
			}
		}
	}

	goMaxProcs := runtime.GOMAXPROCS(0)

	if readyCount != 1 {
		t.Errorf("OnMasterReady called %d times, want 1", readyCount)
	}

	// Initial spawns + at least one recovery spawn
	if spawnCount < goMaxProcs {
		t.Errorf("OnChildSpawn called %d times, want at least %d", spawnCount, goMaxProcs)
	}

	if recoverCount == 0 {
		t.Error("OnChildRecover was never called")
	}

	// Verify order: all initial spawns come before ready
	readyIdx := -1
	firstSpawnIdx := -1
	for i, e := range events {
		if e.name == "ready" {
			readyIdx = i
		}
		if e.name == "spawn" && firstSpawnIdx == -1 {
			firstSpawnIdx = i
		}
	}
	if readyIdx != -1 && firstSpawnIdx != -1 && readyIdx < firstSpawnIdx {
		t.Error("OnMasterReady was called before OnChildSpawn")
	}
}
