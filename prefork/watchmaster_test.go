// This test verifies that prefork children detect master process death
// and exit instead of becoming orphans.
//
// It uses a two-level subprocess chain:
//   test (grandparent) → helper "master" (parent) → helper "child"
//
// The test kills the master and verifies the child exits.

//go:build !windows

package prefork

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	envWatchMasterRole = "FASTHTTP_TEST_WATCHMASTER_ROLE"
	envChildPipeFD     = "FASTHTTP_TEST_CHILD_PIPE_FD"
	envReadyPipeFD     = "FASTHTTP_TEST_READY_PIPE_FD"
)

func init() {
	switch os.Getenv(envWatchMasterRole) {
	case "master":
		helperMaster()
	case "child":
		helperChild()
	}
}

func TestHelperProcess(t *testing.T) {
	// No-op. The real work happens in init().
	// Exists so the test binary can be re-invoked with -test.run=TestHelperProcess.
}

// helperMaster is the intermediate "master" process. It spawns a child
// that runs watchMaster, passes the child's PID back to the test via
// a pipe, then sleeps forever (waiting to be killed).
func helperMaster() {
	// The test passed us pipes as extra files:
	//   fd 3 = pipe to report child PID
	//   fd 4 = pipe the child writes to signal readiness
	pipefd, _ := strconv.Atoi(os.Getenv(envChildPipeFD))
	pipe := os.NewFile(uintptr(pipefd), "pid-pipe")

	readyfd, _ := strconv.Atoi(os.Getenv(envReadyPipeFD))
	readyPipe := os.NewFile(uintptr(readyfd), "ready-pipe")

	// Spawn the child. Filter out existing role env to avoid duplicates.
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, envWatchMasterRole+"=") &&
			!strings.HasPrefix(e, envChildPipeFD+"=") &&
			!strings.HasPrefix(e, envReadyPipeFD+"=") {
			env = append(env, e)
		}
	}
	env = append(env, envWatchMasterRole+"=child")
	// Pass the ready pipe to the child as fd 3.
	env = append(env, envReadyPipeFD+"=3")

	// #nosec G204
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{readyPipe}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	readyPipe.Close()

	// Report child PID to the test process.
	_, _ = pipe.WriteString(strconv.Itoa(cmd.Process.Pid))
	pipe.Close()

	// Wait for the child (or be killed by the test).
	_ = cmd.Wait()
	os.Exit(0)
}

// helperChild runs watchMaster and blocks. If watchMaster works correctly,
// this process will exit when the master (helperMaster) is killed.
func helperChild() {
	p := &Prefork{}
	go p.watchMaster()

	// Signal to the test that we're running and have recorded our PPID.
	readyfd, _ := strconv.Atoi(os.Getenv(envReadyPipeFD))
	readyPipe := os.NewFile(uintptr(readyfd), "ready-pipe")
	_, _ = readyPipe.WriteString("ready")
	readyPipe.Close()

	// Block forever — watchMaster should call os.Exit when the parent dies.
	select {}
}

func Test_watchMaster_detectsParentDeath(t *testing.T) {
	if os.Getenv(envWatchMasterRole) != "" {
		return // skip when running as subprocess helper
	}

	// Pipe for the master to report the child PID.
	pidR, pidW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Pipe for the child to signal it's ready (PPID recorded).
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	// Spawn the master helper.
	// ExtraFiles: fd 3 = pidW, fd 4 = readyW
	// #nosec G204
	master := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	master.Env = append(os.Environ(),
		envWatchMasterRole+"=master",
		envChildPipeFD+"=3",
		envReadyPipeFD+"=4",
	)
	master.ExtraFiles = []*os.File{pidW, readyW}
	master.Stdout = os.Stdout
	master.Stderr = os.Stderr

	if err := master.Start(); err != nil {
		t.Fatalf("failed to start master helper: %v", err)
	}
	pidW.Close()
	readyW.Close()

	// Read the child PID from the master.
	buf := make([]byte, 32)
	n, err := pidR.Read(buf)
	pidR.Close()
	if err != nil {
		t.Fatalf("failed to read child PID: %v", err)
	}
	childPID, err := strconv.Atoi(string(buf[:n]))
	if err != nil {
		t.Fatalf("invalid child PID: %v", err)
	}

	// Wait for the child to signal it's ready.
	readyBuf := make([]byte, 16)
	_, err = readyR.Read(readyBuf)
	readyR.Close()
	if err != nil {
		t.Fatalf("child never signaled readiness: %v", err)
	}

	// Verify child is alive.
	childProc, err := os.FindProcess(childPID)
	if err != nil {
		t.Fatalf("could not find child process %d: %v", childPID, err)
	}
	if err := childProc.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("child process %d is not running: %v", childPID, err)
	}

	// Kill the master (simulating unexpected death).
	if err := master.Process.Kill(); err != nil {
		t.Fatalf("failed to kill master: %v", err)
	}
	_ = master.Wait()

	// Wait for the child to exit (watchMaster polls every 500ms).
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			_ = childProc.Kill()
			t.Fatal("child process did not exit after master was killed — watchMaster failed to detect parent death")
		case <-ticker.C:
			if err := childProc.Signal(syscall.Signal(0)); err != nil {
				// Process is gone — success.
				return
			}
		}
	}
}
