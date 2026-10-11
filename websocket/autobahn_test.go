package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// The Autobahn test suite runs in docker and is skipped unless AUTOBAHN is set.
// AUTOBAHN_CASES narrows the case list, e.g. "1.*,2.*".

const autobahnImage = "crossbario/autobahn-testsuite"

func autobahnCases(t *testing.T) string {
	t.Helper()
	if os.Getenv("AUTOBAHN") == "" {
		t.Skip("set AUTOBAHN=1 to run the Autobahn test suite with docker")
	}
	cases := "1.*,2.*,3.*,4.*,5.*,6.*,7.*,9.*,10.*"
	if v := os.Getenv("AUTOBAHN_CASES"); v != "" {
		cases = v
	}
	return `"` + strings.ReplaceAll(cases, ",", `","`) + `"`
}

func autobahnEcho(c *Conn) {
	ctx := c.Context()
	for {
		typ, r, err := c.Reader(ctx)
		if err != nil {
			return
		}
		w, err := c.Writer(ctx, typ)
		if err != nil {
			return
		}
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				break
			}
		}
		if err := w.Close(); err != nil {
			return
		}
	}
}

// autobahnDirs creates the config and reports directories mounted into the
// container; docker does not create missing host directories on every platform.
func autobahnDirs(t *testing.T, spec, name string) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"config", "reports"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config", name), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// checkAutobahnReport fails on any case not OK, INFORMATIONAL or NON-STRICT,
// on an announced case missing from the report and on fewer than want cases.
func checkAutobahnReport(t *testing.T, path string, announced []string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	var report map[string]map[string]struct {
		Behavior      string `json:"behavior"`
		BehaviorClose string `json:"behaviorClose"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	ok := func(s string) bool { return s == "OK" || s == "INFORMATIONAL" || s == "NON-STRICT" }
	n := 0
	for _, cases := range report {
		for id, r := range cases {
			n++
			if !ok(r.Behavior) || !ok(r.BehaviorClose) {
				t.Errorf("case %s: %s / %s", id, r.Behavior, r.BehaviorClose)
			}
		}
		for _, id := range announced {
			if _, found := cases[id]; !found {
				t.Errorf("case %s missing from the report", id)
			}
		}
	}
	if n < want || n == 0 {
		t.Fatalf("report has %d cases, want %d", n, want)
	}
	t.Logf("%d cases", n)
}

func TestAutobahnServer(t *testing.T) {
	cases := autobahnCases(t)
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	opts := &AcceptOptions{MaxMessageSize: 32 << 20, CheckOrigin: func(*fasthttp.RequestCtx) bool { return true }}
	s := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { _ = Accept(ctx, opts, autobahnEcho) }}
	go func() { _ = s.Serve(ln) }()

	host, dockerNet := "host.docker.internal", []string{"--add-host=host.docker.internal:host-gateway"}
	if runtime.GOOS == "linux" {
		host, dockerNet = "127.0.0.1", []string{"--network=host"}
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address %T", ln.Addr())
	}
	spec := fmt.Sprintf(`{"outdir": "/reports", "servers": [{"agent": "fasthttp", "url": "ws://%s:%d"}], "cases": [%s]}`,
		host, addr.Port, cases)
	dir := autobahnDirs(t, spec, "fuzzingclient.json")
	args := append([]string{"run", "--rm"}, dockerNet...)
	args = append(args, "-v", filepath.Join(dir, "config")+":/config", "-v", filepath.Join(dir, "reports")+":/reports",
		autobahnImage, "wstest", "-m", "fuzzingclient", "-s", "/config/fuzzingclient.json")
	out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput()
	t.Logf("wstest:\n%s", out)
	if err != nil {
		t.Fatalf("docker: %v", err)
	}
	var announced []string
	if m := regexp.MustCompile(`Cases = \[(.*)\]`).FindSubmatch(out); m != nil {
		for _, c := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(m[1], -1) {
			announced = append(announced, string(c[1]))
		}
	}
	if len(announced) == 0 {
		t.Fatal("wstest did not announce its cases")
	}
	checkAutobahnReport(t, filepath.Join(dir, "reports", "index.json"), announced, len(announced))
}

func TestAutobahnClient(t *testing.T) {
	cases := autobahnCases(t)
	spec := fmt.Sprintf(`{"url": "ws://127.0.0.1:9001", "outdir": "/reports", "cases": [%s]}`, cases)
	dir := autobahnDirs(t, spec, "fuzzingserver.json")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	run := exec.CommandContext(t.Context(), "docker", "run", "--rm", "-d", "-p", addr+":9001",
		"-v", filepath.Join(dir, "config")+":/config", "-v", filepath.Join(dir, "reports")+":/reports",
		autobahnImage, "wstest", "-m", "fuzzingserver", "-s", "/config/fuzzingserver.json")
	var stderr bytes.Buffer
	run.Stderr = &stderr
	id, err := run.Output()
	if err != nil {
		t.Fatalf("docker: %v\n%s", err, stderr.Bytes())
	}
	container := strings.TrimSpace(string(id))
	defer func() { _ = exec.Command("docker", "rm", "-f", container).Run() }()

	// wstest requires the Host port to be the one it listens on inside the container.
	var h fasthttp.RequestHeader
	h.SetHost("127.0.0.1:9001")
	opts := &DialOptions{Header: &h, MaxMessageSize: 32 << 20}
	ctx := t.Context()
	var count int
	readyCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		c, err := Dial(readyCtx, "ws://"+addr+"/getCaseCount", opts)
		if err == nil {
			_, p, rerr := c.Read(readyCtx)
			_ = c.CloseNow()
			if rerr == nil {
				count, err = strconv.Atoi(string(p))
			} else {
				err = rerr
			}
		}
		if err == nil {
			break
		}
		if readyCtx.Err() != nil {
			t.Fatalf("fuzzingserver not reachable: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	for i := 1; i <= count; i++ {
		c, err := Dial(ctx, fmt.Sprintf("ws://%s/runCase?case=%d&agent=fasthttp", addr, i), opts)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		autobahnEcho(c)
		_ = c.CloseNow()
	}
	c, err := Dial(ctx, "ws://"+addr+"/updateReports?agent=fasthttp", opts)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err := c.Read(ctx); err != nil {
			break
		}
	}
	_ = c.CloseNow()
	for range 20 {
		if _, err := os.Stat(filepath.Join(dir, "reports", "index.json")); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	checkAutobahnReport(t, filepath.Join(dir, "reports", "index.json"), nil, count)
}
