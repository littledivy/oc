package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// BgSession represents a background bash command session.
type BgSession struct {
	ID        string
	Command   string
	Cmd       *exec.Cmd
	Output    *liveOutputBuffer
	StartTime time.Time
	Done      chan struct{}
	ExitCode  int
	Err       error
}

var bgSessions = make(map[string]*BgSession)
var bgSessionsMu sync.Mutex
var bgSessionCounter int

func bgNextID() string {
	bgSessionCounter++
	return fmt.Sprintf("bg_%d", bgSessionCounter)
}

// bgStart spawns a command in the background with its own process group.
func bgStart(command string) *BgSession {
	id := bgNextID()
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	setSysProcAttr(cmd)

	lb := &liveOutputBuffer{}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		sess := &BgSession{
			ID: id, Command: command, StartTime: time.Now(),
			Done: make(chan struct{}), ExitCode: -1, Err: err,
		}
		close(sess.Done)
		bgSessionsMu.Lock()
		bgSessions[id] = sess
		bgSessionsMu.Unlock()
		return sess
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		sess := &BgSession{
			ID: id, Command: command, StartTime: time.Now(),
			Done: make(chan struct{}), ExitCode: -1, Err: err,
		}
		close(sess.Done)
		bgSessionsMu.Lock()
		bgSessions[id] = sess
		bgSessionsMu.Unlock()
		return sess
	}

	sess := &BgSession{
		ID:        id,
		Command:   command,
		Cmd:       cmd,
		Output:    lb,
		StartTime: time.Now(),
		Done:      make(chan struct{}),
	}

	bgSessionsMu.Lock()
	bgSessions[id] = sess
	bgSessionsMu.Unlock()

	// Stream output in background
	go func() {
		_, _ = io.Copy(lb, pipe)
		err := cmd.Wait()
		sess.Err = err
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				sess.ExitCode = exitErr.ExitCode()
			} else {
				sess.ExitCode = -1
			}
		}
		close(sess.Done)
	}()

	return sess
}

// bgStatus returns a status string for the given session.
func bgStatus(id string) string {
	bgSessionsMu.Lock()
	sess, ok := bgSessions[id]
	bgSessionsMu.Unlock()
	if !ok {
		return fmt.Sprintf("Error: no background session with id %q", id)
	}

	var b strings.Builder
	select {
	case <-sess.Done:
		elapsed := time.Since(sess.StartTime).Round(time.Millisecond)
		fmt.Fprintf(&b, "Session %s: exited (code %d) after %s\n", sess.ID, sess.ExitCode, elapsed)
		if sess.Err != nil && sess.ExitCode == -1 {
			fmt.Fprintf(&b, "Error: %v\n", sess.Err)
		}
	default:
		elapsed := time.Since(sess.StartTime).Round(time.Millisecond)
		fmt.Fprintf(&b, "Session %s: running (%s elapsed)\n", sess.ID, elapsed)
	}

	fmt.Fprintf(&b, "Command: %s\n", sess.Command)
	if sess.Output != nil {
		tail := sess.Output.Snapshot(50)
		if tail != "" {
			fmt.Fprintf(&b, "\n--- output (last 50 lines) ---\n%s\n", tail)
		}
	}
	return b.String()
}

// bgKill kills the process group of the given session.
func bgKill(id string) string {
	bgSessionsMu.Lock()
	sess, ok := bgSessions[id]
	bgSessionsMu.Unlock()
	if !ok {
		return fmt.Sprintf("Error: no background session with id %q", id)
	}

	select {
	case <-sess.Done:
		return fmt.Sprintf("Session %s already exited (code %d)", sess.ID, sess.ExitCode)
	default:
	}

	killCmdGroup(sess.Cmd)
	// Wait briefly for it to finish
	select {
	case <-sess.Done:
	case <-time.After(500 * time.Millisecond):
	}
	return fmt.Sprintf("Session %s killed", sess.ID)
}


