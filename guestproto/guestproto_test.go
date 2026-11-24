package guestproto

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestContainerFramesRoundTrip(t *testing.T) {
	resizePayload, err := json.Marshal(ContainerResizeRequest{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	exitPayload, err := json.Marshal(Exit{Code: 137, Elapsed: int64(time.Second), Signal: "SIGKILL"})
	if err != nil {
		t.Fatal(err)
	}

	frames := []struct {
		kind byte
		data []byte
	}{
		{FrameStdout, []byte("hello stdout\n")},
		{FrameStderr, []byte("hello stderr\n")},
		{FrameStdin, []byte("input\n")},
		{FrameStdinEOF, nil},
		{FrameResize, resizePayload},
		{FrameData, []byte("tar-chunk-bytes")},
		{FrameEOF, nil},
		{FrameEOF, []byte("stream failure")},
		{FrameExit, exitPayload},
	}

	var buf bytes.Buffer
	for _, f := range frames {
		if err := WriteFrame(&buf, f.kind, f.data); err != nil {
			t.Fatalf("WriteFrame(%d): %v", f.kind, err)
		}
	}

	for i, want := range frames {
		kind, got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame[%d]: %v", i, err)
		}
		if kind != want.kind {
			t.Fatalf("frame[%d] kind = %d, want %d", i, kind, want.kind)
		}
		if !bytes.Equal(got, want.data) {
			t.Fatalf("frame[%d] payload = %q, want %q", i, got, want.data)
		}
	}
}

func TestContainerTypesJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 123456789, time.UTC)

	req := Request{
		V:  Version,
		Op: OpContainerStart,
		Container: &ContainerSpec{
			Argv:           []string{"/bin/sh", "-c", "echo hi"},
			Env:            []string{"FOO=bar"},
			Workdir:        "/app",
			User:           "1000:1000",
			Hostname:       "demo",
			ExtraHosts:     []string{"api.local:10.0.0.2"},
			TTY:            true,
			Interactive:    true,
			Rows:           24,
			Cols:           80,
			Privileged:     true,
			ReadonlyRootfs: true,
			MemoryMax:      512 << 20,
			CPUQuota:       100000,
			CPUPeriod:      100000,
			PidsLimit:      256,
			Layers: []LayerSource{
				{Device: "/dev/vdb"},
				{File: "/mnt/layers/abc.erofs"},
			},
			Mounts: []ContainerMount{
				{Kind: "bind", Source: "/mnt/b0", Target: "/data", ReadOnly: true},
				{Kind: "volume", Source: "/dev/vdc", Target: "/var/lib/db"},
				{Kind: "tmpfs", Target: "/tmp", Options: "size=64m,mode=1777"},
			},
		},
		ContainerExec: &ContainerExecRequest{
			Argv:        []string{"id"},
			Env:         []string{"TERM=xterm-256color"},
			Workdir:     "/",
			User:        "root",
			TTY:         true,
			Interactive: true,
			Rows:        30,
			Cols:        100,
			Privileged:  true,
		},
		ContainerSignal: &ContainerSignalRequest{Signal: "SIGTERM", PID: 1, All: true},
		ContainerResize: &ContainerResizeRequest{Rows: 42, Cols: 132},
		ContainerCopy:   &ContainerCopyRequest{Path: "/etc/hosts", Archive: true, FollowLink: true, NoOverwriteDir: true, User: "0:0"},
		ContainerExport: &ContainerExportRequest{Path: "/", UpperOnly: true},
	}

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var gotReq Request
	if err := json.Unmarshal(b, &gotReq); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotReq, req) {
		t.Fatalf("Request round-trip mismatch:\ngot:  %+v\nwant: %+v", gotReq, req)
	}

	resp := Response{
		OK: true,
		Hello: &Hello{
			Protocol:    Version,
			MinProtocol: Version,
			Agent:       "2",
			Kernel:      "6.18.64-0-virt",
			Machine:     "aarch64",
			Rosetta:     true,
			Uptime:      1.25,
			Features:    []string{FeatureContainers},
		},
		Container: &ContainerState{
			Running:  true,
			Paused:   false,
			PID:      1,
			ExitCode: 0,
			Signal:   "",
		},
		ExecPID: 42,
		Processes: []ContainerProcess{{
			PID:      1,
			PPID:     0,
			UID:      0,
			GID:      0,
			State:    "S",
			Threads:  2,
			CPUNanos: 5000,
			RSSBytes: 4096,
			Comm:     "sh",
			Cmdline:  []string{"/bin/sh"},
		}},
		Stats: &ContainerStats{
			Timestamp:        now,
			CPUUserNanos:     100,
			CPUSystemNanos:   200,
			CPUPeriods:       10,
			CPUThrottled:     1,
			CPUThrottledNs:   50,
			MemoryBytes:      1024,
			MemoryAnonBytes:  512,
			MemoryFileBytes:  256,
			MemoryTmpfsBytes: 128,
			MemoryLimitBytes: 1 << 30,
			Network: []ContainerNetStats{{
				Interface: "eth0",
				RxBytes:   1000,
				TxBytes:   2000,
				RxPackets: 10,
				TxPackets: 20,
			}},
			PIDs:      3,
			PIDsLimit: 100,
		},
		PathStat: &ContainerPathStat{
			Name:       "hosts",
			Size:       123,
			Mode:       0o644,
			Mtime:      now,
			LinkTarget: "",
		},
	}

	b, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var gotResp Response
	if err := json.Unmarshal(b, &gotResp); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotResp, resp) {
		t.Fatalf("Response round-trip mismatch:\ngot:  %+v\nwant: %+v", gotResp, resp)
	}
	if !gotResp.Hello.HasFeature(FeatureContainers) || gotResp.Hello.HasFeature("other") {
		t.Fatalf("Hello.HasFeature mismatch: %+v", gotResp.Hello.Features)
	}
}

func TestV1BackwardsCompatibility(t *testing.T) {
	// Existing v1 hello/exec JSON without new fields must still decode cleanly.
	raw := `{"v":1,"op":"exec","exec":{"argv":["/bin/true"],"stdin":true}}`
	var req Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if req.V != 1 || req.Op != OpExec || req.Exec == nil || len(req.Exec.Argv) != 1 || !req.Exec.Stdin {
		t.Fatalf("unexpected v1 decode: %+v", req)
	}
}
