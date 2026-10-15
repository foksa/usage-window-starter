package core

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// setupShortHome is setup with a short data folder: a Unix socket path must fit in
// about 100 bytes, and t.TempDir's paths on macOS don't leave room for app.sock.
func setupShortHome(t *testing.T) {
	t.Helper()
	setup(t)
	dir, err := os.MkdirTemp("", "uws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	SetHome(dir)
}

func TestRequestShowIsAnswered(t *testing.T) {
	setupShortHome(t)
	shown := make(chan struct{}, 1)
	ln, err := ServeRequests(Handlers{Show: func() { shown <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := RequestShow(time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-shown:
	default:
		t.Fatal("answered without showing")
	}
}

func TestRequestShowFromHungCopy(t *testing.T) {
	setupShortHome(t)
	block := make(chan struct{})
	defer close(block)
	ln, err := ServeRequests(Handlers{Show: func() { <-block }})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := RequestShow(200 * time.Millisecond); !errors.Is(err, ErrNotResponding) {
		t.Fatalf("err = %v, want ErrNotResponding", err)
	}
}

func TestRequestStartReturnsResult(t *testing.T) {
	setupShortHome(t)
	window := LimitWindow{Active: true, UsedPct: 0, ResetsAt: ms(nowMs() + 5*3600_000)}
	var asked Provider
	ln, err := ServeRequests(Handlers{Start: func(p Provider) StartResult {
		asked = p
		return StartResult{FiveHour: &window}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r, err := RequestStart(Codex, time.Second)
	if err != nil || asked != Codex || r.Error != "" || r.FiveHour == nil || *r.FiveHour.ResetsAt != *window.ResetsAt {
		t.Fatalf("asked=%q r=%+v err=%v", asked, r, err)
	}
}

// No one listening (an older copy without the socket) is not taken for a hung copy.
func TestRequestShowWithoutListener(t *testing.T) {
	setupShortHome(t)
	err := RequestShow(time.Second)
	if err == nil || errors.Is(err, ErrNotResponding) {
		t.Fatalf("err = %v, want a dial error", err)
	}
}

// A hung copy that holds the lock and ignores SIGTERM is killed and replaced.
func TestStopInstanceReplacesHungCopy(t *testing.T) {
	if os.Getenv("UWS_HUNG_COPY") != "" {
		signal.Ignore(syscall.SIGTERM)
		if _, _, err := AcquireInstanceLock(); err != nil {
			os.Exit(1)
		}
		os.Stdout.WriteString("locked\n")
		time.Sleep(time.Hour) // not select{}: the runtime would exit it as deadlocked
	}
	setup(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestStopInstanceReplacesHungCopy$")
	cmd.Env = append(os.Environ(), "UWS_HUNG_COPY=1", "USAGE_WINDOW_STARTER_HOME="+HomeDir)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }() // reaps it, so the pid stops existing
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	buf := make([]byte, 7)
	if _, err := out.Read(buf); err != nil || string(buf) != "locked\n" {
		t.Fatalf("child didn't take the lock: %q %v", buf, err)
	}
	_, pid, err := AcquireInstanceLock()
	if err != ErrAlreadyRunning || pid != cmd.Process.Pid {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
	lock, err := StopInstance(pid)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}
