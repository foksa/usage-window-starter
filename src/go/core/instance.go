package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrAlreadyRunning means another copy of the app holds the instance lock.
var ErrAlreadyRunning = errors.New("already running")

// ErrNotResponding means the running copy took the show request but never answered:
// it's hung (for example after WindowServer restarted under it, which takes its menu
// bar item).
var ErrNotResponding = errors.New("running copy is not responding")

func lockFile() string   { return filepath.Join(HomeDir, "app.lock") }
func socketFile() string { return filepath.Join(HomeDir, "app.sock") }

// AcquireInstanceLock makes sure only one copy of the app runs per data folder.
// It holds an exclusive flock on app.lock for the life of the process (the OS
// drops it on exit or crash) and records this process's pid in it. When another
// copy holds the lock it returns that copy's pid and ErrAlreadyRunning.
func AcquireInstanceLock() (*os.File, int, error) {
	return acquireLock(syscall.LOCK_EX | syscall.LOCK_NB)
}

func acquireLock(how int) (*os.File, int, error) {
	if err := os.MkdirAll(HomeDir, 0o755); err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(lockFile(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, 0, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		data, _ := os.ReadFile(lockFile())
		f.Close()
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, pid, ErrAlreadyRunning
		}
		return nil, pid, err
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, 0, nil
}

// Handlers are what the running copy does for another copy of the app: Show when the
// app is opened again, Start for the CLI's "start" (so the session is started by the
// copy that owns the state, not next to it).
type Handlers struct {
	Show  func()
	Start func(Provider) StartResult
}

// StartResult is how a start went: the provider's error, if any, and its 5h window.
type StartResult struct {
	Error    string       `json:"error,omitempty"`
	FiveHour *LimitWindow `json:"fiveHour,omitempty"`
}

// request is one JSON line sent over app.sock; the answer is one JSON line back.
type request struct {
	Cmd      string   `json:"cmd"` // "show" or "start"
	Provider Provider `json:"provider,omitempty"`
}

// ServeRequests answers other copies of the app on app.sock. A copy whose UI is stuck
// still accepts (this runs off the main thread) but never answers a show request,
// and that is how a second copy tells it's hung. Call it only while holding the
// instance lock, which makes removing a leftover socket file safe.
func ServeRequests(h Handlers) (net.Listener, error) {
	_ = os.Remove(socketFile())
	ln, err := net.Listen("unix", socketFile())
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // closed
			}
			go func() {
				defer conn.Close()
				var req request
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				var reply any = struct{}{}
				switch req.Cmd {
				case "show":
					h.Show()
				case "start":
					reply = h.Start(req.Provider)
				default:
					reply = StartResult{Error: fmt.Sprintf("unknown request %q", req.Cmd)}
				}
				_ = json.NewEncoder(conn).Encode(reply)
			}()
		}
	}()
	return ln, nil
}

// RequestShow asks the running copy to show its panel. It returns ErrNotResponding
// when that copy took the request but didn't answer within timeout. Any other error
// means it couldn't be reached at all, e.g. it's an older version without the
// socket; that's no proof it's hung.
func RequestShow(timeout time.Duration) error {
	var reply struct{}
	return ask(request{Cmd: "show"}, timeout, &reply)
}

// RequestStart has the running copy start p's session and waits for the result,
// which takes a couple of minutes (see Timing.ConfirmDelay).
func RequestStart(p Provider, timeout time.Duration) (StartResult, error) {
	var r StartResult
	err := ask(request{Cmd: "start", Provider: p}, timeout, &r)
	return r, err
}

// ask sends req to the running copy and decodes its answer into reply.
func ask(req request, timeout time.Duration, reply any) error {
	conn, err := dial(3 * time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	if json.NewDecoder(conn).Decode(reply) != nil {
		return ErrNotResponding
	}
	return nil
}

// dial connects to the running copy, retrying for up to wait: a copy that has
// just started holds the lock a moment before it listens.
func dial(wait time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := net.Dial("unix", socketFile())
		if err == nil || time.Now().After(deadline) {
			return conn, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// StopInstance ends the hung copy with pid and takes its instance lock. It asks
// politely first, then uses SIGKILL (a copy whose main thread is stuck may ignore
// SIGTERM). The lock is released by the OS when that copy exits, so getting it is
// how we know it's gone.
func StopInstance(pid int) (*os.File, error) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if f, err := waitForLock(2 * time.Second); err == nil {
		return f, nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return waitForLock(3 * time.Second)
}

// waitForLock blocks on the instance lock for up to timeout.
func waitForLock(timeout time.Duration) (*os.File, error) {
	type result struct {
		f   *os.File
		err error
	}
	got := make(chan result, 1)
	go func() {
		f, _, err := acquireLock(syscall.LOCK_EX)
		got <- result{f, err}
	}()
	select {
	case r := <-got:
		return r.f, r.err
	case <-time.After(timeout):
		// The blocked flock is left behind; if it ever succeeds, it closes the file.
		go func() {
			if r := <-got; r.f != nil {
				r.f.Close()
			}
		}()
		return nil, ErrAlreadyRunning
	}
}
