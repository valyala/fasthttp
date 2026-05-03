package prefork

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// noopChildProducer returns a CommandProducer that re-execs the test binary
// into a no-op subprocess. The returned cleanup must be deferred (or registered
// via t.Cleanup) so leaked subprocesses are reaped if the test fails midway.
func noopChildProducer(t testing.TB) (func(files []*os.File) (*exec.Cmd, error), func()) {
	t.Helper()
	var (
		mu      sync.Mutex
		spawned []*exec.Cmd
	)

	produce := func(_ []*os.File) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), preforkChildEnvVariable+"="+preforkChildEnvValue)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		mu.Lock()
		spawned = append(spawned, cmd)
		mu.Unlock()
		return cmd, nil
	}

	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, cmd := range spawned {
			if cmd == nil || cmd.Process == nil {
				continue
			}
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}
	return produce, cleanup
}

func Test_IsChild(t *testing.T) {
	// This test cannot run in parallel — IsChild() reads a process-global env var.
	if IsChild() {
		t.Fatal("test starts as child unexpectedly")
	}

	t.Setenv(preforkChildEnvVariable, preforkChildEnvValue)
	if !IsChild() {
		t.Errorf("IsChild() == false after Setenv, want true")
	}
}

func Test_setTCPListenerFiles(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	p := &Prefork{}
	addr := "127.0.0.1:0"

	if err := p.setTCPListenerFiles(addr); err != nil {
		t.Fatalf("setTCPListenerFiles: %v", err)
	}
	t.Cleanup(func() {
		_ = p.ln.Close()
		for _, f := range p.files {
			_ = f.Close()
		}
	})

	if p.ln == nil {
		t.Fatal("p.ln is nil after setTCPListenerFiles")
	}
	if got := p.ln.Addr().String(); got == "" {
		t.Error("p.ln.Addr() is empty")
	}
	if len(p.files) != 1 {
		t.Errorf("len(p.files) == %d, want 1", len(p.files))
	}
}

// Test_ListenAndServe_Stub_ChildPath drives the child branch of all three
// ListenAndServe* entry points using a stubbed Serve function. It replaces the
// previous trio of near-identical tests that only validated field assignment.
func Test_ListenAndServe_Stub_ChildPath(t *testing.T) {
	// child env mutation precludes t.Parallel.
	t.Setenv(preforkChildEnvVariable, preforkChildEnvValue)

	type call struct {
		listener bool
		certFile string
		keyFile  string
		certData string
		keyData  string
	}

	tests := []struct {
		name string
		run  func(t *testing.T, p *Prefork, addr string) error
		want call
	}{
		{
			name: "ListenAndServe",
			run:  func(_ *testing.T, p *Prefork, addr string) error { return p.ListenAndServe(addr) },
			want: call{listener: true},
		},
		{
			name: "ListenAndServeTLS",
			run: func(_ *testing.T, p *Prefork, addr string) error {
				return p.ListenAndServeTLS(addr, "./key", "./cert")
			},
			want: call{listener: true, certFile: "./cert", keyFile: "./key"},
		},
		{
			name: "ListenAndServeTLSEmbed",
			run: func(_ *testing.T, p *Prefork, addr string) error {
				return p.ListenAndServeTLSEmbed(addr, []byte("certPEM"), []byte("keyPEM"))
			},
			want: call{listener: true, certData: "certPEM", keyData: "keyPEM"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got call
			p := New(&fasthttp.Server{})
			p.Reuseport = true
			p.ServeFunc = func(ln net.Listener) error {
				got.listener = ln != nil
				return nil
			}
			p.ServeTLSFunc = func(ln net.Listener, certFile, keyFile string) error {
				got.listener = ln != nil
				got.certFile = certFile
				got.keyFile = keyFile
				return nil
			}
			p.ServeTLSEmbedFunc = func(ln net.Listener, certData, keyData []byte) error {
				got.listener = ln != nil
				got.certData = string(certData)
				got.keyData = string(keyData)
				return nil
			}

			addr := "127.0.0.1:0"
			if err := tc.run(t, p, addr); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			t.Cleanup(func() {
				if p.ln != nil {
					_ = p.ln.Close()
				}
			})

			if got != tc.want {
				t.Errorf("%s call = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

func Test_doCommand_CommandProducerErrors(t *testing.T) {
	t.Parallel()

	producerErr := errors.New("boom")
	tests := []struct {
		name    string
		produce func(files []*os.File) (*exec.Cmd, error)
		wantErr error
	}{
		{
			name: "producer returns error",
			produce: func([]*os.File) (*exec.Cmd, error) {
				return nil, producerErr
			},
			wantErr: producerErr,
		},
		{
			name: "producer returns nil cmd",
			//nolint:nilnil // intentionally tests the (nil, nil) misbehaviour guard
			produce: func([]*os.File) (*exec.Cmd, error) {
				return nil, nil
			},
			wantErr: ErrCommandProducerNilCmd,
		},
		{
			name: "producer returns unstarted cmd",
			produce: func([]*os.File) (*exec.Cmd, error) {
				return &exec.Cmd{}, nil
			},
			wantErr: ErrCommandProducerNotStarted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &Prefork{CommandProducer: tc.produce}
			cmd, err := p.doCommand()
			if cmd != nil {
				t.Errorf("expected nil cmd on error, got %v", cmd)
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

type testLogger struct {
	mu       sync.Mutex
	messages []string
}

func (l *testLogger) Printf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.messages = append(l.messages, msg)
	l.mu.Unlock()
}

// Test_Prefork_Lifecycle drives prefork() to ErrOverRecovery via
// short-lived no-op children and asserts the callback ordering / arguments.
func Test_Prefork_Lifecycle(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

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

	produce, cleanup := noopChildProducer(t)
	t.Cleanup(cleanup)

	p := &Prefork{
		Reuseport:        true,
		RecoverThreshold: 1,
		Logger:           &testLogger{},
		CommandProducer:  produce,
		OnChildSpawn: func(pid int) error {
			record("spawn", pid)
			return nil
		},
		OnMasterReady: func(childPIDs []int) error {
			record("ready", childPIDs...)
			return nil
		},
		OnChildRecover: func(oldPID, newPID int) {
			record("recover", oldPID, newPID)
		},
	}

	err := p.prefork("127.0.0.1:0")
	if !errors.Is(err, ErrOverRecovery) {
		t.Fatalf("expected ErrOverRecovery, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	goMaxProcs := runtime.GOMAXPROCS(0)

	var spawnCount, readyCount, recoverCount int
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

	if readyCount != 1 {
		t.Errorf("OnMasterReady called %d times, want 1", readyCount)
	}
	if spawnCount < goMaxProcs {
		t.Errorf("OnChildSpawn called %d times, want at least %d", spawnCount, goMaxProcs)
	}
	if recoverCount == 0 {
		t.Error("OnChildRecover was never called")
	}

	// ready must come after exactly goMaxProcs initial spawns.
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

	// every recover event must be preceded by a spawn for the new PID.
	recoveredSpawnByPID := make(map[int]bool)
	for _, e := range events[readyIdx+1:] {
		if e.name == "spawn" {
			recoveredSpawnByPID[e.pids[0]] = true
		}
		if e.name == "recover" {
			if !recoveredSpawnByPID[e.pids[1]] {
				t.Errorf("OnChildRecover for PID %d happened before OnChildSpawn", e.pids[1])
			}
		}
	}
}

func Test_Prefork_InitialChildSpawnError(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	produce, cleanup := noopChildProducer(t)
	t.Cleanup(cleanup)

	expectedErr := errors.New("initial spawn rejected")
	var calls atomic.Int32

	p := &Prefork{
		Reuseport:        true,
		RecoverThreshold: 1,
		Logger:           &testLogger{},
		CommandProducer:  produce,
		OnChildSpawn: func(_ int) error {
			calls.Add(1)
			return expectedErr
		},
	}

	err := p.prefork("127.0.0.1:0")
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected %v, got: %v", expectedErr, err)
	}
	if calls.Load() == 0 {
		t.Fatal("OnChildSpawn was never invoked")
	}
}

func Test_Prefork_OnMasterReadyError(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	produce, cleanup := noopChildProducer(t)
	t.Cleanup(cleanup)

	expectedErr := errors.New("ready rejected")
	p := &Prefork{
		Reuseport:        true,
		RecoverThreshold: 1,
		Logger:           &testLogger{},
		CommandProducer:  produce,
		OnMasterReady: func([]int) error {
			return expectedErr
		},
	}

	err := p.prefork("127.0.0.1:0")
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected %v, got: %v", expectedErr, err)
	}
}

func Test_Prefork_RecoveredChildSpawnError(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	produce, cleanup := noopChildProducer(t)
	t.Cleanup(cleanup)

	expectedErr := errors.New("spawn failed")
	var spawnCount, recoverCount atomic.Int32

	p := &Prefork{
		Reuseport:        true,
		RecoverThreshold: 1,
		Logger:           &testLogger{},
		CommandProducer:  produce,
		OnChildSpawn: func(pid int) error {
			if pid <= 0 {
				t.Errorf("OnChildSpawn called with invalid PID: %d", pid)
			}
			n := spawnCount.Add(1)
			if int(n) > runtime.GOMAXPROCS(0) {
				return expectedErr
			}
			return nil
		},
		OnChildRecover: func(_, _ int) {
			recoverCount.Add(1)
		},
	}

	err := p.prefork("127.0.0.1:0")
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected %v, got: %v", expectedErr, err)
	}
	if got := recoverCount.Load(); got != 0 {
		t.Fatalf("OnChildRecover called %d times, want 0", got)
	}
}

// Test_Prefork_RecoverInterval verifies the optional backoff delays the respawn.
func Test_Prefork_RecoverInterval(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	produce, cleanup := noopChildProducer(t)
	t.Cleanup(cleanup)

	const interval = 50 * time.Millisecond
	p := &Prefork{
		Reuseport:        true,
		RecoverThreshold: 1,
		RecoverInterval:  interval,
		Logger:           &testLogger{},
		CommandProducer:  produce,
	}

	start := time.Now()
	err := p.prefork("127.0.0.1:0")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrOverRecovery) {
		t.Fatalf("expected ErrOverRecovery, got %v", err)
	}
	// At least one recover interval must have elapsed before threshold fired.
	if elapsed < interval {
		t.Errorf("elapsed %v < interval %v; backoff did not apply", elapsed, interval)
	}
}
