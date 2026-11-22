// Package proto is what the vzrosetta host program and the agent in the
// guest say to each other over a virtio-vsock connection.
//
// A connection opens with one request, a line of JSON. What follows
// depends on its Op:
//
//   - "ping": the agent answers one line and closes.
//   - "exec": the agent runs Argv and sends frames: the command's output
//     as it comes, then one exit frame.
//   - "dial": the agent connects to the Unix socket Addr in the guest,
//     answers the line "ok" (or "error: …"), and from then on the
//     connection carries that socket's bytes both ways.
package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// AgentPort is the vsock port the agent listens on in the guest.
const AgentPort = 1024

// The requests an agent answers.
const (
	OpPing = "ping"
	OpExec = "exec"
	OpDial = "dial"
)

// Request opens a connection to the agent.
type Request struct {
	Op string `json:"op"`
	// Argv is the command of an exec; Env is added to the agent's own
	// environment and Dir is where it runs.
	Argv []string `json:"argv,omitempty"`
	Env  []string `json:"env,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	// Addr is the Unix socket of a dial.
	Addr string `json:"addr,omitempty"`
}

// WriteRequest sends req as the line that opens a connection.
func WriteRequest(w io.Writer, req Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadRequest reads the line WriteRequest sent.
func ReadRequest(r *bufio.Reader) (Request, error) {
	var req Request
	line, err := r.ReadBytes('\n')
	if err != nil {
		return req, err
	}
	return req, json.Unmarshal(line, &req)
}

// The kinds of frame an exec answers with.
const (
	FrameStdout byte = 1
	FrameStderr byte = 2
	// FrameExit ends an exec; its payload is what FormatExit made.
	FrameExit byte = 3
)

// WriteFrame sends one frame: its kind, its length, its bytes.
func WriteFrame(w io.Writer, kind byte, p []byte) error {
	hdr := [5]byte{kind}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	if _, err := w.Write(append(hdr[:], p...)); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads the frame WriteFrame sent.
func ReadFrame(r io.Reader) (kind byte, p []byte, err error) {
	var hdr [5]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	p = make([]byte, binary.BigEndian.Uint32(hdr[1:]))
	_, err = io.ReadFull(r, p)
	return hdr[0], p, err
}

// FormatExit is the payload of an exit frame: the exit code, how long the
// command ran as the agent saw it, and why it could not be run, if so.
func FormatExit(code int, elapsed time.Duration, problem string) []byte {
	return fmt.Appendf(nil, "%d %d %s", code, elapsed.Nanoseconds(), problem)
}

// ParseExit reads what FormatExit made.
func ParseExit(p []byte) (code int, elapsed time.Duration, problem string, err error) {
	parts := strings.SplitN(string(p), " ", 3)
	if len(parts) != 3 {
		return 0, 0, "", fmt.Errorf("exit frame %q", p)
	}
	if code, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, "", err
	}
	ns, err := strconv.ParseInt(parts[1], 10, 64)
	return code, time.Duration(ns), parts[2], err
}
