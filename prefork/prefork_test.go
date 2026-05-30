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
	"time"

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
	// This test can't run parallel as it modifies the process environment.

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
	prev := runtime.GOMAXPROCS(0)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(prev)
	})

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

func Test_preforkClosesParentListenerFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	prev := runtime.GOMAXPROCS(1)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(prev)
	})

	stopErr := errors.New("stop prefork")
	var parentFile *os.File
	p := &Prefork{
		CommandProducer: func(files []*os.File) (*exec.Cmd, error) {
			if len(files) != 1 {
				t.Fatalf("unexpected files count: %d. Expecting 1", len(files))
			}
			if _, err := files[0].Stat(); err != nil {
				t.Fatalf("listener file must be open while starting child: %v", err)
			}
			parentFile = files[0]

			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), preforkChildEnvVariable+"=1")
			err := cmd.Start()
			return cmd, err
		},
		OnChildSpawn: func(pid int) error {
			return stopErr
		},
	}

	if err := p.prefork(getAddr()); !errors.Is(err, stopErr) {
		t.Fatalf("Unexpected error: %v. Expecting %v", err, stopErr)
	}
	if parentFile == nil {
		t.Fatal("listener file was not passed to child command")
	}
	if _, err := parentFile.Stat(); err == nil {
		t.Fatal("parent listener file was not closed")
	}
}

func Test_setTCPListenerFilesClosesListenerOnFileError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	fileErr := errors.New("file error")
	oldTCPListenerFile := tcpListenerFile
	tcpListenerFile = func(*net.TCPListener) (*os.File, error) {
		return nil, fileErr
	}
	t.Cleanup(func() {
		tcpListenerFile = oldTCPListenerFile
	})

	p := &Prefork{}
	err := p.setTCPListenerFiles(getAddr())
	if !errors.Is(err, fileErr) {
		t.Fatalf("Unexpected error: %v. Expecting %v", err, fileErr)
	}
	if p.files != nil {
		t.Fatalf("Prefork.files = %v, want nil", p.files)
	}
	if p.ln == nil {
		return
	}

	closeErrCh := make(chan error, 1)
	go func() {
		closeErrCh <- p.ln.Close()
	}()
	select {
	case err := <-closeErrCh:
		if err == nil {
			t.Fatal("listener remained open after File error")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout closing listener")
	}
}

func Test_ListenAndServe(t *testing.T) {
	// This test can't run parallel as it modifies the process environment.

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
	// This test can't run parallel as it modifies the process environment.

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
	// This test can't run parallel as it modifies the process environment.

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

type testLogger struct {
	messages []string
}

func (l *testLogger) Printf(format string, args ...any) {
	l.messages = append(l.messages, fmt.Sprintf(format, args...))
}

// Test_Prefork_Lifecycle runs the full prefork lifecycle with a CommandProducer
// and verifies that callbacks are invoked in the correct order with the correct arguments.
func Test_Prefork_Lifecycle(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(prev)
	})

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
		CommandProducer: func(_ []*os.File) (*exec.Cmd, error) {
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

	readyIdx := -1
	spawnsBeforeReady := 0
	for i, e := range events {
		if e.name == "ready" {
			readyIdx = i
			break
		}
		if e.name == "spawn" {
			spawnsBeforeReady++
		}
	}

	if readyIdx == -1 {
		t.Fatal("OnMasterReady was never called")
	}
	if spawnsBeforeReady != goMaxProcs {
		t.Errorf("OnMasterReady called after %d initial spawns, want %d", spawnsBeforeReady, goMaxProcs)
	}

	recoveredSpawnByPID := make(map[int]bool)
	recoveredPIDs := make(map[int]bool)
	for _, e := range events[readyIdx+1:] {
		if e.name == "spawn" {
			recoveredSpawnByPID[e.pids[0]] = true
		}
		if e.name == "recover" {
			if !recoveredSpawnByPID[e.pids[1]] {
				t.Errorf("OnChildRecover for PID %d happened before OnChildSpawn", e.pids[1])
			}
			recoveredPIDs[e.pids[1]] = true
		}
	}
	for pid := range recoveredPIDs {
		if !recoveredSpawnByPID[pid] {
			t.Errorf("OnChildRecover for PID %d did not have a matching OnChildSpawn", pid)
		}
	}
}

func Test_Prefork_RecoveredChildSpawnError(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(prev)
	})

	expectedErr := errors.New("spawn failed")
	var spawnCount int
	var recoverCount int

	p := &Prefork{
		Reuseport:        true,
		RecoverThreshold: 1,
		Logger:           &testLogger{},
		CommandProducer: func(_ []*os.File) (*exec.Cmd, error) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), preforkChildEnvVariable+"=1")
			err := cmd.Start()
			return cmd, err
		},
		OnChildSpawn: func(pid int) error {
			if pid <= 0 {
				t.Errorf("OnChildSpawn called with invalid PID: %d", pid)
			}
			spawnCount++
			if spawnCount > runtime.GOMAXPROCS(0) {
				return expectedErr
			}
			return nil
		},
		OnChildRecover: func(_, _ int) {
			recoverCount++
		},
	}

	err := p.prefork(getAddr())
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected %v, got: %v", expectedErr, err)
	}
	if recoverCount != 0 {
		t.Fatalf("OnChildRecover called %d times, want 0", recoverCount)
	}
}
