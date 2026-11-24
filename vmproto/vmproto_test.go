package vmproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
)

func TestVMSpecSharesAndExtraDisksRoundTrip(t *testing.T) {
	spec := VMSpec{
		Kernel:    "/assets/Image",
		Initrd:    "/assets/initramfs.gz",
		CPUs:      4,
		MemoryMiB: 1024,
		Disk:      &DiskSpec{Path: "/tmp/rootfs.ext4", Size: 16 << 30},
		Rosetta:   false,
		Network:   true,
		Shares: []ShareSpec{
			{Tag: "layers", Dir: "/cache/layers", ReadOnly: true},
			{Tag: "b0", Dir: "/Users/alice/work", ReadOnly: false},
		},
		ExtraDisks: []ExtraDiskSpec{
			{Path: "/cache/layers/abc.erofs", ReadOnly: true},
			{Path: "/volumes/data.ext4", ReadOnly: false},
		},
	}

	var buf bytes.Buffer
	req := Request{
		V:     Version,
		Op:    OpStart,
		Start: &StartRequest{Spec: spec},
	}
	if err := WriteLine(&buf, req); err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := ReadLine(bufio.NewReader(&buf), &got); err != nil {
		t.Fatal(err)
	}
	if got.Start == nil || !reflect.DeepEqual(got.Start.Spec, spec) {
		t.Fatalf("VMSpec round-trip mismatch:\ngot:  %+v\nwant: %+v", got.Start, spec)
	}
}

func TestContainerOpsAndTypesAliasGuestProto(t *testing.T) {
	// Ensure vmproto container types are identical aliases of guestproto types.
	var gs guestproto.ContainerSpec = ContainerSpec{
		Argv:   []string{"/bin/sh"},
		Layers: []LayerSource{{Device: "/dev/vdb"}},
	}
	var vs ContainerSpec = gs

	req := Request{
		V:               Version,
		Op:              OpContainerStart,
		Container:       &vs,
		ContainerSignal: &ContainerSignalRequest{Signal: "SIGTERM"},
		ContainerResize: &ContainerResizeRequest{Rows: 24, Cols: 80},
		ContainerExec:   &ContainerExecRequest{Argv: []string{"ls"}, TTY: true},
		ContainerCopy:   &ContainerCopyRequest{Path: "/etc/hostname"},
		ContainerExport: &ContainerExportRequest{UpperOnly: true},
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
		t.Fatalf("Request mismatch:\ngot:  %+v\nwant: %+v", gotReq, req)
	}

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	resp := Response{
		OK:        true,
		Hello:     &Hello{Protocol: Version, MinProtocol: Version, Features: []string{FeatureContainers}},
		Container: &ContainerState{Running: true, PID: 1},
		ExecPID:   7,
		Processes: []ContainerProcess{{PID: 1, Comm: "sh"}},
		Stats:     &ContainerStats{Timestamp: now, MemoryBytes: 4096, PIDs: 1},
		PathStat:  &ContainerPathStat{Name: "hostname", Size: 5, Mode: 0o644, Mtime: now},
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
		t.Fatalf("Response mismatch:\ngot:  %+v\nwant: %+v", gotResp, resp)
	}
	if !gotResp.Hello.HasFeature(FeatureContainers) {
		t.Fatalf("expected Hello.HasFeature(%q) == true", FeatureContainers)
	}
}
