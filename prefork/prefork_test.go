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

func Test_Prefork_OnMasterDeath(t *testing.T) {
	t.Parallel()

	var called bool
	p := &Prefork{
		OnMasterDeath: func() {
			called = true
		},
	}

	if p.OnMasterDeath == nil {
		t.Error("OnMasterDeath should not be nil")
	}

	p.OnMasterDeath()
	if !called {
		t.Error("OnMasterDeath was not called")
	}
}

func Test_Prefork_Callbacks_NotNil(t *testing.T) {
	t.Parallel()

	var spawnCalled bool
	var readyCalled bool
	var recoverCalled bool

	p := &Prefork{
		OnChildSpawn: func(pid int) error {
			spawnCalled = true
			return nil
		},
		OnMasterReady: func(childPIDs []int) error {
			readyCalled = true
			return nil
		},
		OnChildRecover: func(pid int) error {
			recoverCalled = true
			return nil
		},
	}

	// Test that callbacks are set
	if p.OnChildSpawn == nil {
		t.Error("OnChildSpawn should not be nil")
	}
	if p.OnMasterReady == nil {
		t.Error("OnMasterReady should not be nil")
	}
	if p.OnChildRecover == nil {
		t.Error("OnChildRecover should not be nil")
	}

	// Test that callbacks can be called
	_ = p.OnChildSpawn(1234)
	_ = p.OnMasterReady([]int{1234, 5678})
	_ = p.OnChildRecover(9999)

	if !spawnCalled {
		t.Error("OnChildSpawn was not called")
	}
	if !readyCalled {
		t.Error("OnMasterReady was not called")
	}
	if !recoverCalled {
		t.Error("OnChildRecover was not called")
	}
}

func Test_Prefork_Callbacks_Nil(t *testing.T) {
	t.Parallel()

	// Test that nil callbacks don't panic when checked
	p := &Prefork{}

	if p.OnChildSpawn != nil {
		t.Error("OnChildSpawn should be nil by default")
	}
	if p.OnMasterReady != nil {
		t.Error("OnMasterReady should be nil by default")
	}
	if p.OnChildRecover != nil {
		t.Error("OnChildRecover should be nil by default")
	}
}

func Test_Prefork_RecoverThreshold(t *testing.T) {
	t.Parallel()

	s := &fasthttp.Server{}
	p := New(s)

	// Default should be GOMAXPROCS/2
	expected := runtime.GOMAXPROCS(0) / 2
	if p.RecoverThreshold != expected {
		t.Errorf("RecoverThreshold == %d, want %d", p.RecoverThreshold, expected)
	}

	// Test custom threshold
	p.RecoverThreshold = 10
	if p.RecoverThreshold != 10 {
		t.Errorf("RecoverThreshold == %d, want %d", p.RecoverThreshold, 10)
	}
}

func Test_ErrOverRecovery(t *testing.T) {
	t.Parallel()

	if ErrOverRecovery == nil {
		t.Error("ErrOverRecovery should not be nil")
	}
	if ErrOverRecovery.Error() != "exceeding the value of RecoverThreshold" {
		t.Errorf("ErrOverRecovery message incorrect: %s", ErrOverRecovery.Error())
	}
}

func Test_ErrOnlyReuseportOnWindows(t *testing.T) {
	t.Parallel()

	if ErrOnlyReuseportOnWindows == nil {
		t.Error("ErrOnlyReuseportOnWindows should not be nil")
	}
	if ErrOnlyReuseportOnWindows.Error() != "windows only supports Reuseport = true" {
		t.Errorf("ErrOnlyReuseportOnWindows message incorrect: %s", ErrOnlyReuseportOnWindows.Error())
	}
}

func Test_Listen_ChildCreatesListener(t *testing.T) {
	// This test can't run parallel as it modifies env.

	setUp()
	defer tearDown()

	p := &Prefork{
		Reuseport: true,
	}
	addr := getAddr()

	ln, err := p.listen(addr)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	defer ln.Close()

	if ln == nil {
		t.Error("Listener should not be nil")
	}
}

func Test_OnChildSpawn_Error(t *testing.T) {
	t.Parallel()

	errExpected := errors.New("spawn callback error")
	p := &Prefork{
		OnChildSpawn: func(pid int) error {
			return errExpected
		},
	}

	// Test that error is returned correctly
	err := p.OnChildSpawn(1234)
	if err != errExpected {
		t.Errorf("OnChildSpawn error == %v, want %v", err, errExpected)
	}
}

func Test_OnMasterReady_Error(t *testing.T) {
	t.Parallel()

	errExpected := errors.New("master ready callback error")
	p := &Prefork{
		OnMasterReady: func(childPIDs []int) error {
			return errExpected
		},
	}

	// Test that error is returned correctly
	err := p.OnMasterReady([]int{1, 2, 3})
	if err != errExpected {
		t.Errorf("OnMasterReady error == %v, want %v", err, errExpected)
	}
}

func Test_OnMasterReady_ReceivesPIDs(t *testing.T) {
	t.Parallel()

	var receivedPIDs []int
	p := &Prefork{
		OnMasterReady: func(childPIDs []int) error {
			receivedPIDs = childPIDs
			return nil
		},
	}

	expectedPIDs := []int{100, 200, 300}
	_ = p.OnMasterReady(expectedPIDs)

	if len(receivedPIDs) != len(expectedPIDs) {
		t.Errorf("Received %d PIDs, want %d", len(receivedPIDs), len(expectedPIDs))
	}

	for i, pid := range expectedPIDs {
		if receivedPIDs[i] != pid {
			t.Errorf("PID[%d] == %d, want %d", i, receivedPIDs[i], pid)
		}
	}
}

func Test_CommandProducer(t *testing.T) {
	t.Parallel()

	var producerCalled bool
	p := &Prefork{
		CommandProducer: func(files []*os.File) (*exec.Cmd, error) {
			producerCalled = true
			// Re-exec the test binary with a no-op flag for hermetic testing
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.ExtraFiles = files
			cmd.Env = append(os.Environ(), preforkChildEnvVariable+"=1")
			err := cmd.Start()
			return cmd, err
		},
	}

	if p.CommandProducer == nil {
		t.Error("CommandProducer should not be nil")
	}

	cmd, err := p.doCommand()
	if err != nil {
		t.Fatalf("doCommand failed: %v", err)
	}

	_ = cmd.Wait()

	if !producerCalled {
		t.Error("CommandProducer was not called")
	}
}

func Test_CommandProducer_Nil_UsesDefault(t *testing.T) {
	t.Parallel()

	p := &Prefork{}

	// Verify default CommandProducer is nil
	if p.CommandProducer != nil {
		t.Error("CommandProducer should be nil by default")
	}
}
