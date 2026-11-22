package vmhelper

import (
	"io"
	"os"
	"path/filepath"
)

const (
	consoleFileName = "console.log"
	maxConsoleBytes = 1 << 20 // 1 MiB cap per VM start
)

// consoleSink captures guest hvc0 output to <dir>/console.log, rotating the
// previous file once to console.log.1 and capping writes at 1 MiB while
// draining anything beyond that so the guest never stalls.
type consoleSink struct {
	path string
	w    *os.File
	done chan struct{}
}

func openConsoleSink(dir string) (*consoleSink, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, consoleFileName)
	if _, err := os.Stat(path); err == nil {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer r.Close()
		defer f.Close()
		_, _ = io.CopyN(f, r, maxConsoleBytes)
		_, _ = io.Copy(io.Discard, r)
	}()
	return &consoleSink{path: path, w: w, done: done}, nil
}

// File returns the write end passed to vz.Config.Console.
func (c *consoleSink) File() *os.File {
	if c == nil {
		return nil
	}
	return c.w
}

// Path returns the console log file path.
func (c *consoleSink) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// CloseWriter closes the helper's handle to the pipe write end once VZ has
// duplicated it (or on startup failure).
func (c *consoleSink) CloseWriter() {
	if c != nil && c.w != nil {
		_ = c.w.Close()
		c.w = nil
	}
}
