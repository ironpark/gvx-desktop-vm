package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

func TestBuildLowerDir(t *testing.T) {
	if got := buildLowerDir(nil); got != "" {
		t.Fatalf("empty lowerdir = %q", got)
	}
	if got := buildLowerDir([]string{"/mnt/l0"}); got != "/mnt/l0" {
		t.Fatalf("single layer = %q", got)
	}
	// Layers are passed bottom-to-top; overlayfs expects topmost layer first.
	if got := buildLowerDir([]string{"/mnt/l0", "/mnt/l1", "/mnt/l2"}); got != "/mnt/l2:/mnt/l1:/mnt/l0" {
		t.Fatalf("multi layer = %q, want /mnt/l2:/mnt/l1:/mnt/l0", got)
	}
}

func TestValidateContainerSpec(t *testing.T) {
	valid := &guestproto.ContainerSpec{
		Argv:   []string{"/bin/sh"},
		Layers: []guestproto.LayerSource{{Device: "/dev/vdb"}, {File: "/mnt/layers/abc.erofs"}},
		Mounts: []guestproto.ContainerMount{
			{Kind: "bind", Source: "/mnt/b0", Target: "/app", ReadOnly: true},
			{Kind: "volume", Source: "/dev/vdc", Target: "/data"},
			{Kind: "tmpfs", Target: "/tmp", Options: "size=64m"},
		},
	}
	if err := validateContainerSpec(valid); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}

	badCases := []struct {
		name string
		spec *guestproto.ContainerSpec
	}{
		{"nil", nil},
		{"empty argv", &guestproto.ContainerSpec{Layers: []guestproto.LayerSource{{Device: "/dev/vdb"}}}},
		{"empty layers", &guestproto.ContainerSpec{Argv: []string{"/bin/sh"}}},
		{"layer neither dev nor file", &guestproto.ContainerSpec{Argv: []string{"sh"}, Layers: []guestproto.LayerSource{{}}}},
		{"layer both dev and file", &guestproto.ContainerSpec{Argv: []string{"sh"}, Layers: []guestproto.LayerSource{{Device: "/dev/vdb", File: "/mnt/layers/x"}}}},
		{"layer bad dev", &guestproto.ContainerSpec{Argv: []string{"sh"}, Layers: []guestproto.LayerSource{{Device: "/tmp/x"}}}},
		{"layer rel file", &guestproto.ContainerSpec{Argv: []string{"sh"}, Layers: []guestproto.LayerSource{{File: "rel.erofs"}}}},
		{"mount bad kind", &guestproto.ContainerSpec{Argv: []string{"sh"}, Layers: []guestproto.LayerSource{{Device: "/dev/vdb"}}, Mounts: []guestproto.ContainerMount{{Kind: "nfs", Source: "/a", Target: "/b"}}}},
		{"mount missing src", &guestproto.ContainerSpec{Argv: []string{"sh"}, Layers: []guestproto.LayerSource{{Device: "/dev/vdb"}}, Mounts: []guestproto.ContainerMount{{Kind: "bind", Target: "/b"}}}},
		{"mount rel target", &guestproto.ContainerSpec{Argv: []string{"sh"}, Layers: []guestproto.LayerSource{{Device: "/dev/vdb"}}, Mounts: []guestproto.ContainerMount{{Kind: "tmpfs", Target: "tmp"}}}},
		{"mount root target", &guestproto.ContainerSpec{Argv: []string{"sh"}, Layers: []guestproto.LayerSource{{Device: "/dev/vdb"}}, Mounts: []guestproto.ContainerMount{{Kind: "tmpfs", Target: "/"}}}},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateContainerSpec(tc.spec); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestResolveRootfsPath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Absolute symlink inside rootfs must resolve inside rootfs, not host /etc/hosts!
	if err := os.Symlink("/etc/hosts", filepath.Join(root, "etc", "abslink")); err != nil {
		t.Fatal(err)
	}
	// Escaping relative symlink must clamp at rootfs!
	if err := os.Symlink("../../../../../../etc/hosts", filepath.Join(root, "etc", "sub", "escape")); err != nil {
		t.Fatal(err)
	}
	// Symlink loop must error.
	if err := os.Symlink("loopb", filepath.Join(root, "etc", "loopa")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loopa", filepath.Join(root, "etc", "loopb")); err != nil {
		t.Fatal(err)
	}

	got, err := resolveRootfsPath(root, "/etc/../../../etc/hosts", true)
	want := filepath.Join(root, "etc", "hosts")
	if err != nil || got != want {
		t.Fatalf("traversal clamp = (%q, %v), want %q", got, err, want)
	}

	got, err = resolveRootfsPath(root, "/etc/abslink", true)
	if err != nil || got != want {
		t.Fatalf("abs symlink follow = (%q, %v), want %q", got, err, want)
	}

	got, err = resolveRootfsPath(root, "/etc/abslink", false)
	if err != nil || got != filepath.Join(root, "etc", "abslink") {
		t.Fatalf("abs symlink nofollow = (%q, %v)", got, err)
	}

	got, err = resolveRootfsPath(root, "/etc/sub/escape", true)
	if err != nil || got != want {
		t.Fatalf("escape symlink = (%q, %v), want %q", got, err, want)
	}

	if _, err := resolveRootfsPath(root, "/etc/loopa", true); err == nil {
		t.Fatalf("expected error on symlink loop")
	}
}

func TestParseSignalName(t *testing.T) {
	cases := []struct {
		in       string
		wantNum  int
		wantName string
		wantErr  bool
	}{
		{"", 9, "SIGKILL", false},
		{"TERM", 15, "SIGTERM", false},
		{"SIGTERM", 15, "SIGTERM", false},
		{"sigint", 2, "SIGINT", false},
		{"9", 9, "SIGKILL", false},
		{"1", 1, "SIGHUP", false},
		{"HUP", 1, "SIGHUP", false},
		{"USR1", 10, "SIGUSR1", false},
		{"0", 0, "", true},
		{"99", 0, "", true},
		{"NOPE", 0, "", true},
	}
	for _, tc := range cases {
		num, name, err := parseSignalName(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSignalName(%q) = (%d, %q), want error", tc.in, num, name)
			}
			continue
		}
		if err != nil || num != tc.wantNum || name != tc.wantName {
			t.Errorf("parseSignalName(%q) = (%d, %q, %v), want (%d, %q)", tc.in, num, name, err, tc.wantNum, tc.wantName)
		}
	}
}

func TestRenderHostsAndGateway(t *testing.T) {
	route := strings.Join([]string{
		"Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT",
		"eth0\t00000000\t0140A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0",
		"eth0\t0040A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0",
	}, "\n")
	if gw := parseDefaultGateway(route); gw != "192.168.64.1" {
		t.Fatalf("gateway = %q, want 192.168.64.1", gw)
	}

	hosts := renderHostsFile("mybox", "192.168.64.1", []string{
		"api.local:10.0.0.5",
		"gw.local:host-gateway",
		"v6.local:fd00::1",
		"bad\nline:1.2.3.4",
	})
	for _, want := range []string{
		"127.0.0.1\tlocalhost\n",
		"127.0.0.1\tmybox\n",
		"192.168.64.1\thost.docker.internal\n",
		"10.0.0.5\tapi.local\n",
		"192.168.64.1\tgw.local\n",
		"fd00::1\tv6.local\n",
	} {
		if !strings.Contains(hosts, want) {
			t.Errorf("hosts missing %q:\n%s", want, hosts)
		}
	}
	if strings.Contains(hosts, "1.2.3.4") {
		t.Errorf("hosts must sanitize newline injection:\n%s", hosts)
	}
}

func TestParseCgroupStatsAndProcProcess(t *testing.T) {
	cpuStat := "usage_usec 35000\nuser_usec 20000\nsystem_usec 15000\nnr_periods 50\nnr_throttled 2\nthrottled_usec 4000\n"
	memStat := "anon 4096000\nfile 2048000\nshmem 1024000\n"
	memInfo := "MemTotal:        1048576 kB\nMemFree:          524288 kB\n"
	netDev := strings.Join([]string{
		"Inter-|   Receive                                                |  Transmit",
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed",
		"    lo:   1000      10    0    0    0     0          0         0     1000      10    0    0    0     0       0          0",
		"  eth0: 123456     100    0    0    0     0          0         0   654321     200    0    0    0     0       0          0",
	}, "\n")

	st := parseCgroupStats(cpuStat, "8192000\n", memStat, "max\n", memInfo, "3\n", "128\n", netDev)
	if st.CPUUserNanos != 20_000_000 || st.CPUSystemNanos != 15_000_000 || st.CPUPeriods != 50 || st.CPUThrottled != 2 || st.CPUThrottledNs != 4_000_000 {
		t.Fatalf("unexpected cpu stats: %+v", st)
	}
	if st.MemoryBytes != 8192000 || st.MemoryAnonBytes != 4096000 || st.MemoryFileBytes != 2048000 || st.MemoryTmpfsBytes != 1024000 || st.MemoryLimitBytes != 1048576*1024 {
		t.Fatalf("unexpected memory stats: %+v", st)
	}
	if st.PIDs != 3 || st.PIDsLimit != 128 {
		t.Fatalf("unexpected pids stats: %+v", st)
	}
	if len(st.Network) != 1 || st.Network[0].Interface != "eth0" || st.Network[0].RxBytes != 123456 || st.Network[0].TxBytes != 654321 || st.Network[0].RxPackets != 100 || st.Network[0].TxPackets != 200 {
		t.Fatalf("unexpected net stats: %+v", st.Network)
	}

	// Process with spaces and parentheses in comm.
	statLine := "42 (my (proc) 1) S 1 42 42 0 -1 4194304 10 0 0 0 12 8 0 0 20 0 4 0 100 10000 50 18446744073709551615"
	statusData := "Name:\tmy (proc) 1\nUid:\t1000\t1000\t1000\t1000\nGid:\t100\t100\t100\t100\nThreads:\t4\n"
	cmdline := []byte("/usr/bin/app\x00--flag\x00val\x00")
	proc, ok := parseProcProcess(42, statLine, statusData, cmdline, 100, 4096)
	if !ok {
		t.Fatal("parseProcProcess failed")
	}
	if proc.PID != 42 || proc.PPID != 1 || proc.Comm != "my (proc) 1" || proc.State != "S" ||
		proc.UID != 1000 || proc.GID != 100 || proc.Threads != 4 ||
		proc.CPUNanos != 200_000_000 || proc.RSSBytes != 50*4096 ||
		!slices.Equal(proc.Cmdline, []string{"/usr/bin/app", "--flag", "val"}) {
		t.Fatalf("unexpected proc: %+v", proc)
	}
}

func TestOutputHubBufferedAndLive(t *testing.T) {
	hub := newOutputHub(64)
	hub.Append(guestproto.FrameStdout, []byte("hello "))
	hub.Append(guestproto.FrameStderr, []byte("world"))

	// Live subscriber gets buffered chunks first, then live chunk, then exit.
	var gotOut, gotErr bytes.Buffer
	done := make(chan guestproto.Exit, 1)
	stop := make(chan struct{})
	go func() {
		ex, exited, err := hub.Stream(stop, func(kind byte, p []byte) error {
			switch kind {
			case guestproto.FrameStdout:
				gotOut.Write(p)
			case guestproto.FrameStderr:
				gotErr.Write(p)
			}
			return nil
		})
		if err != nil || !exited {
			t.Errorf("stream err=%v exited=%v", err, exited)
		}
		done <- ex
	}()

	time.Sleep(15 * time.Millisecond)
	hub.Append(guestproto.FrameStdout, []byte("!"))
	hub.Finish(guestproto.Exit{Code: 42})

	select {
	case ex := <-done:
		if ex.Code != 42 || gotOut.String() != "hello !" || gotErr.String() != "world" {
			t.Fatalf("live got out=%q err=%q exit=%+v", gotOut.String(), gotErr.String(), ex)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream")
	}

	// Late subscriber after exit still receives buffered output + exit.
	var late bytes.Buffer
	ex, exited, err := hub.Stream(stop, func(kind byte, p []byte) error {
		late.Write(p)
		return nil
	})
	if err != nil || !exited || ex.Code != 42 || late.String() != "hello world!" {
		t.Fatalf("late subscriber got %q exit=%+v err=%v", late.String(), ex, err)
	}
}

func TestTarHelpersCopyAndExport(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"app", "proc/1", "sys/class", "dev/pts"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "app", "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "1", "cmdline"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(root, "app", "link.txt")); err != nil {
		t.Fatal(err)
	}

	// Stat symlink with follow=false vs follow=true.
	st, _, err := statContainerPath(root, "/app/link.txt", false)
	if err != nil || st.LinkTarget != "hello.txt" || st.Name != "link.txt" {
		t.Fatalf("stat symlink nofollow = (%+v, %v)", st, err)
	}
	st2, _, err := statContainerPath(root, "/app/link.txt", true)
	if err != nil || st2.LinkTarget != "" || st2.Size != 2 {
		t.Fatalf("stat symlink follow = (%+v, %v)", st2, err)
	}

	// CopyFrom directory includes basename.
	var copyBuf bytes.Buffer
	if err := writeTarFromPath(filepath.Join(root, "app"), true, false, false, &copyBuf); err != nil {
		t.Fatalf("writeTarFromPath copy: %v", err)
	}
	names := listTarNames(t, copyBuf.Bytes())
	if !slices.Contains(names, "app/hello.txt") || !slices.Contains(names, "app/link.txt") {
		t.Fatalf("copy tar entries = %v", names)
	}

	// Export rootfs keeps empty proc/sys/dev without descending into proc/1.
	var exportBuf bytes.Buffer
	if err := writeTarFromPath(root, false, false, true, &exportBuf); err != nil {
		t.Fatalf("writeTarFromPath export: %v", err)
	}
	expNames := listTarNames(t, exportBuf.Bytes())
	if !slices.Contains(expNames, "app/hello.txt") || !slices.Contains(expNames, "proc/") {
		t.Fatalf("export tar entries = %v", expNames)
	}
	if slices.Contains(expNames, "proc/1/cmdline") {
		t.Fatalf("export descended into /proc: %v", expNames)
	}

	// CopyTo extracts cleanly and enforces NoOverwriteDir.
	dstRoot := t.TempDir()
	dstDir := filepath.Join(dstRoot, "dest")
	if err := os.MkdirAll(filepath.Join(dstDir, "existingdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTarToDir(dstRoot, dstDir, false, bytes.NewReader(copyBuf.Bytes())); err != nil {
		t.Fatalf("extractTarToDir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dstDir, "app", "hello.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("extracted file = (%q, %v)", data, err)
	}

	// Overwriting directory with file when NoOverwriteDir=true must fail.
	var overwriteBuf bytes.Buffer
	tw := tar.NewWriter(&overwriteBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "existingdir", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	if err := extractTarToDir(dstRoot, dstDir, true, bytes.NewReader(overwriteBuf.Bytes())); err == nil {
		t.Fatalf("expected NoOverwriteDir error")
	}

	// Security: reject traversal, absolute paths, NUL bytes, symlink-then-write
	// destDir escapes, and hardlink escapes.
	if _, err := cleanTarRelPath("ok\x00bad"); err == nil {
		t.Fatalf("cleanTarRelPath accepted NUL byte")
	}
	makeTarBytes := func(t *testing.T, entries []tar.Header, bodies map[string]string) []byte {
		t.Helper()
		var buf bytes.Buffer
		w := tar.NewWriter(&buf)
		for _, h := range entries {
			body := bodies[h.Name]
			if h.Typeflag == tar.TypeReg {
				h.Size = int64(len(body))
			}
			if h.Mode == 0 {
				h.Mode = 0o644
			}
			if err := w.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
			if len(body) > 0 {
				if _, err := w.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	secCases := []struct {
		name    string
		entries []tar.Header
		bodies  map[string]string
		outside string
	}{
		{
			name:    "dotdot_traversal",
			entries: []tar.Header{{Name: "../../etc/trav.txt", Typeflag: tar.TypeReg}},
			bodies:  map[string]string{"../../etc/trav.txt": "bad"},
			outside: "etc/trav.txt",
		},
		{
			name:    "nested_dotdot_traversal",
			entries: []tar.Header{{Name: "a/../../escape.txt", Typeflag: tar.TypeReg}},
			bodies:  map[string]string{"a/../../escape.txt": "bad"},
			outside: "escape.txt",
		},
		{
			name:    "absolute_path",
			entries: []tar.Header{{Name: "/etc/abs.txt", Typeflag: tar.TypeReg}},
			bodies:  map[string]string{"/etc/abs.txt": "bad"},
			outside: "etc/abs.txt",
		},
		{
			name: "symlink_then_write_escape_destDir",
			entries: []tar.Header{
				{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
				{Name: "escape/pwned", Typeflag: tar.TypeReg},
			},
			bodies:  map[string]string{"escape/pwned": "bad"},
			outside: "etc/pwned",
		},
		{
			name: "relative_symlink_escape_destDir",
			entries: []tar.Header{
				{Name: "rel", Typeflag: tar.TypeSymlink, Linkname: "../etc"},
				{Name: "rel/pwned2", Typeflag: tar.TypeReg},
			},
			bodies:  map[string]string{"rel/pwned2": "bad"},
			outside: "etc/pwned2",
		},
		{
			name: "hardlink_dotdot_escape",
			entries: []tar.Header{
				{Name: "hl", Typeflag: tar.TypeLink, Linkname: "../etc/secret"},
			},
		},
		{
			name: "hardlink_absolute_escape",
			entries: []tar.Header{
				{Name: "hl", Typeflag: tar.TypeLink, Linkname: "/etc/secret"},
			},
		},
		{
			name: "hardlink_via_symlink_escape",
			entries: []tar.Header{
				{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
				{Name: "hl", Typeflag: tar.TypeLink, Linkname: "escape/secret"},
			},
		},
	}

	for _, tc := range secCases {
		t.Run("reject_"+tc.name, func(t *testing.T) {
			sRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(sRoot, "etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sRoot, "etc", "secret"), []byte("topsecret"), 0o644); err != nil {
				t.Fatal(err)
			}
			work := filepath.Join(sRoot, "work")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			raw := makeTarBytes(t, tc.entries, tc.bodies)
			if err := extractTarToDir(sRoot, work, false, bytes.NewReader(raw)); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if tc.outside != "" {
				if _, err := os.Lstat(filepath.Join(sRoot, filepath.FromSlash(tc.outside))); err == nil {
					t.Fatalf("malicious tar %s wrote outside destDir: %s", tc.name, tc.outside)
				}
			}
		})
	}
}

func listTarNames(t *testing.T, data []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var out []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		out = append(out, hdr.Name)
	}
}

// fakeContainerOps implements containerOps for portable Server protocol tests.
type fakeContainerOps struct {
	mu            sync.Mutex
	started       bool
	running       bool
	paused        bool
	hub           *outputHub
	stdinBuf      bytes.Buffer
	stdinEOF      bool
	lastResize    guestproto.ContainerResizeRequest
	lastSignal    guestproto.ContainerSignalRequest
	execProcFn    func(req guestproto.ContainerExecRequest) (containerExecProc, error)
	rootfsDir     string
	shutdownCalls atomic.Int32
}

func newFakeContainerOps(t *testing.T) *fakeContainerOps {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0o644)
	return &fakeContainerOps{rootfsDir: dir}
}

func (f *fakeContainerOps) Start(spec guestproto.ContainerSpec) (guestproto.ContainerState, error) {
	if err := validateContainerSpec(&spec); err != nil {
		return guestproto.ContainerState{}, errWithCode(guestproto.CodeBadRequest, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	f.running = true
	f.hub = newOutputHub(defaultOutputBufBytes)
	f.hub.Append(guestproto.FrameStdout, []byte("booted\n"))
	return guestproto.ContainerState{Running: true, PID: 101}, nil
}

func (f *fakeContainerOps) Attach() (guestproto.ContainerState, *outputHub, func([]byte) error, func() error, func(rows, cols uint16) error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		return guestproto.ContainerState{}, nil, nil, nil, nil, errWithCode(guestproto.CodeNotRunning, errors.New("not started"))
	}
	writeStdin := func(p []byte) error {
		f.mu.Lock()
		f.stdinBuf.Write(p)
		f.mu.Unlock()
		return nil
	}
	closeStdin := func() error {
		f.mu.Lock()
		f.stdinEOF = true
		f.mu.Unlock()
		return nil
	}
	return guestproto.ContainerState{Running: f.running, Paused: f.paused, PID: 101}, f.hub, writeStdin, closeStdin, f.Resize, nil
}

func (f *fakeContainerOps) Signal(req guestproto.ContainerSignalRequest) error {
	if _, _, err := parseSignalName(req.Signal); err != nil {
		return errWithCode(guestproto.CodeBadRequest, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return errWithCode(guestproto.CodeNotRunning, errors.New("not running"))
	}
	f.lastSignal = req
	return nil
}

func (f *fakeContainerOps) Resize(rows, cols uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return errWithCode(guestproto.CodeNotRunning, errors.New("not running"))
	}
	f.lastResize = guestproto.ContainerResizeRequest{Rows: rows, Cols: cols}
	return nil
}

func (f *fakeContainerOps) Exec(req guestproto.ContainerExecRequest) (containerExecProc, error) {
	f.mu.Lock()
	running, paused := f.running, f.paused
	fn := f.execProcFn
	f.mu.Unlock()
	if !running {
		return nil, errWithCode(guestproto.CodeNotRunning, errors.New("not running"))
	}
	if paused {
		return nil, errWithCode(guestproto.CodePaused, errors.New("paused"))
	}
	if fn != nil {
		return fn(req)
	}
	return newFakeExecProc(202), nil
}

func (f *fakeContainerOps) Top() ([]guestproto.ContainerProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return nil, errWithCode(guestproto.CodeNotRunning, errors.New("not running"))
	}
	return []guestproto.ContainerProcess{{PID: 101, PPID: 1, Comm: "sh", State: "S"}}, nil
}

func (f *fakeContainerOps) Stats() (guestproto.ContainerStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return guestproto.ContainerStats{}, errWithCode(guestproto.CodeNotRunning, errors.New("not running"))
	}
	return guestproto.ContainerStats{PIDs: 1, MemoryBytes: 1024}, nil
}

func (f *fakeContainerOps) Pause() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return errWithCode(guestproto.CodeNotRunning, errors.New("not running"))
	}
	if f.paused {
		return errWithCode(guestproto.CodeAlreadyPaused, errors.New("already paused"))
	}
	f.paused = true
	return nil
}

func (f *fakeContainerOps) Unpause() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return errWithCode(guestproto.CodeNotRunning, errors.New("not running"))
	}
	if !f.paused {
		return errWithCode(guestproto.CodeNotPaused, errors.New("not paused"))
	}
	f.paused = false
	return nil
}

func (f *fakeContainerOps) Stat(req guestproto.ContainerCopyRequest) (guestproto.ContainerPathStat, error) {
	st, _, err := statContainerPath(f.rootfsDir, req.Path, req.FollowLink)
	return st, err
}

func (f *fakeContainerOps) CopyFrom(req guestproto.ContainerCopyRequest) (guestproto.ContainerPathStat, func(io.Writer) error, error) {
	st, p, err := statContainerPath(f.rootfsDir, req.Path, req.FollowLink)
	if err != nil {
		return guestproto.ContainerPathStat{}, nil, err
	}
	return st, func(w io.Writer) error {
		return writeTarFromPath(p, req.Path != "/", false, false, w)
	}, nil
}

func (f *fakeContainerOps) CopyTo(req guestproto.ContainerCopyRequest) (func(io.Reader) error, error) {
	dest, err := resolveRootfsPath(f.rootfsDir, req.Path, true)
	if err != nil {
		return nil, errWithCode(guestproto.CodeBadRequest, err)
	}
	return func(r io.Reader) error {
		return extractTarToDir(f.rootfsDir, dest, req.NoOverwriteDir, r)
	}, nil
}

func (f *fakeContainerOps) Export(req guestproto.ContainerExportRequest) (func(io.Writer) error, error) {
	return func(w io.Writer) error {
		return writeTarFromPath(f.rootfsDir, false, req.UpperOnly, false, w)
	}, nil
}

func (f *fakeContainerOps) Shutdown(timeout time.Duration) error {
	f.shutdownCalls.Add(1)
	f.mu.Lock()
	f.running = false
	if f.hub != nil {
		f.hub.Finish(guestproto.Exit{Code: 0})
	}
	f.mu.Unlock()
	return nil
}

type fakeExecProc struct {
	pid        int
	mu         sync.Mutex
	stdin      bytes.Buffer
	stdinDone  chan struct{}
	stdinOnce  sync.Once
	lastResize guestproto.ContainerResizeRequest
}

func newFakeExecProc(pid int) *fakeExecProc {
	return &fakeExecProc{pid: pid, stdinDone: make(chan struct{})}
}

func (p *fakeExecProc) PID() int { return p.pid }
func (p *fakeExecProc) WriteStdin(b []byte) error {
	p.mu.Lock()
	p.stdin.Write(b)
	p.mu.Unlock()
	return nil
}
func (p *fakeExecProc) CloseStdin() error {
	p.stdinOnce.Do(func() { close(p.stdinDone) })
	return nil
}
func (p *fakeExecProc) Resize(r, c uint16) error {
	p.mu.Lock()
	p.lastResize = guestproto.ContainerResizeRequest{Rows: r, Cols: c}
	p.mu.Unlock()
	return nil
}
func (p *fakeExecProc) StreamOutput(writeFrame func(byte, []byte) error) {
	<-p.stdinDone
	p.mu.Lock()
	in := p.stdin.String()
	p.mu.Unlock()
	_ = writeFrame(guestproto.FrameStdout, []byte("echo:"+in))
	_ = writeFrame(guestproto.FrameStderr, []byte("err"))
}
func (p *fakeExecProc) Wait() guestproto.Exit {
	<-p.stdinDone
	return guestproto.Exit{Code: 0, Elapsed: 1000}
}
func (p *fakeExecProc) Close() {
	_ = p.CloseStdin()
}

func TestServerContainerOpsProtocol(t *testing.T) {
	ops := newFakeOps()
	ctr := newFakeContainerOps(t)
	srv := NewServer("2", ops)
	srv.ctr = ctr
	srv.shutdownDelay = time.Millisecond

	// 1. Hello advertises FeatureContainers.
	resp := roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpHello})
	if !resp.OK || resp.Hello == nil || !resp.Hello.HasFeature(guestproto.FeatureContainers) {
		t.Fatalf("hello missing FeatureContainers: %+v", resp.Hello)
	}

	// 2. Top before start returns CodeNotRunning.
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerTop})
	if resp.OK || resp.Code != guestproto.CodeNotRunning {
		t.Fatalf("top before start = %+v, want not-running", resp)
	}

	// 3. ContainerStart succeeds.
	resp = roundTrip(t, srv, guestproto.Request{
		V:  guestproto.Version,
		Op: guestproto.OpContainerStart,
		Container: &guestproto.ContainerSpec{
			Argv:   []string{"/bin/sh"},
			Layers: []guestproto.LayerSource{{Device: "/dev/vdb"}},
		},
	})
	if !resp.OK || resp.Container == nil || !resp.Container.Running || resp.Container.PID != 101 {
		t.Fatalf("container-start = %+v", resp)
	}

	// 4. Signal, Resize, Top, Stats, Pause, Unpause.
	resp = roundTrip(t, srv, guestproto.Request{
		V:               guestproto.Version,
		Op:              guestproto.OpContainerSignal,
		ContainerSignal: &guestproto.ContainerSignalRequest{Signal: "SIGTERM"},
	})
	if !resp.OK {
		t.Fatalf("signal = %+v", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{
		V:               guestproto.Version,
		Op:              guestproto.OpContainerResize,
		ContainerResize: &guestproto.ContainerResizeRequest{Rows: 40, Cols: 120},
	})
	if !resp.OK {
		t.Fatalf("resize = %+v", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerTop})
	if !resp.OK || len(resp.Processes) != 1 || resp.Processes[0].PID != 101 {
		t.Fatalf("top = %+v", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerStats})
	if !resp.OK || resp.Stats == nil || resp.Stats.PIDs != 1 {
		t.Fatalf("stats = %+v", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerPause})
	if !resp.OK {
		t.Fatalf("pause = %+v", resp)
	}
	// Duplicate pause -> CodeAlreadyPaused; Exec while paused -> CodePaused.
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerPause})
	if resp.OK || resp.Code != guestproto.CodeAlreadyPaused {
		t.Fatalf("dup pause = %+v, want already-paused", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{
		V:             guestproto.Version,
		Op:            guestproto.OpContainerExec,
		ContainerExec: &guestproto.ContainerExecRequest{Argv: []string{"ls"}},
	})
	if resp.OK || resp.Code != guestproto.CodePaused {
		t.Fatalf("exec while paused = %+v, want paused", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerUnpause})
	if !resp.OK {
		t.Fatalf("unpause = %+v", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerUnpause})
	if resp.OK || resp.Code != guestproto.CodeNotPaused {
		t.Fatalf("dup unpause = %+v, want not-paused", resp)
	}

	// 5. ContainerExec bidirectional framing (ExecPID + stdin/resize + stdout/stderr/exit).
	{
		var ep *fakeExecProc
		ctr.execProcFn = func(req guestproto.ContainerExecRequest) (containerExecProc, error) {
			ep = newFakeExecProc(303)
			return ep, nil
		}
		client, server := net.Pipe()
		go srv.ServeConn(server)
		if err := vmproto.WriteLine(client, guestproto.Request{
			V:             guestproto.Version,
			Op:            guestproto.OpContainerExec,
			ContainerExec: &guestproto.ContainerExecRequest{Argv: []string{"cat"}, Interactive: true, TTY: true},
		}); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(client)
		var r guestproto.Response
		if err := vmproto.ReadLine(br, &r); err != nil || !r.OK || r.ExecPID != 303 {
			t.Fatalf("exec resp = (%+v, %v)", r, err)
		}
		resizePayload, _ := json.Marshal(guestproto.ContainerResizeRequest{Rows: 30, Cols: 100})
		_ = guestproto.WriteFrame(client, guestproto.FrameResize, resizePayload)
		_ = guestproto.WriteFrame(client, guestproto.FrameStdin, []byte("ping"))
		_ = guestproto.WriteFrame(client, guestproto.FrameStdinEOF, nil)

		var out, errOut bytes.Buffer
		var exit guestproto.Exit
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				t.Fatalf("exec frame: %v", err)
			}
			if kind == guestproto.FrameStdout {
				out.Write(payload)
			} else if kind == guestproto.FrameStderr {
				errOut.Write(payload)
			} else if kind == guestproto.FrameExit {
				_ = json.Unmarshal(payload, &exit)
				break
			}
		}
		_ = client.Close()
		if out.String() != "echo:ping" || errOut.String() != "err" || exit.Code != 0 {
			t.Fatalf("exec stream out=%q err=%q exit=%+v", out.String(), errOut.String(), exit)
		}
		ep.mu.Lock()
		gotResize := ep.lastResize
		ep.mu.Unlock()
		if gotResize.Rows != 30 || gotResize.Cols != 100 {
			t.Fatalf("exec resize = %+v", gotResize)
		}
	}

	// 6. Stat, CopyFrom, CopyTo, Export.
	resp = roundTrip(t, srv, guestproto.Request{
		V:             guestproto.Version,
		Op:            guestproto.OpContainerStat,
		ContainerCopy: &guestproto.ContainerCopyRequest{Path: "/hello.txt"},
	})
	if !resp.OK || resp.PathStat == nil || resp.PathStat.Size != 5 || resp.PathStat.Name != "hello.txt" {
		t.Fatalf("stat = %+v", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{
		V:             guestproto.Version,
		Op:            guestproto.OpContainerStat,
		ContainerCopy: &guestproto.ContainerCopyRequest{Path: "/missing"},
	})
	if resp.OK || resp.Code != guestproto.CodeNotFound {
		t.Fatalf("stat missing = %+v, want not-found", resp)
	}

	// CopyFrom
	{
		client, server := net.Pipe()
		go srv.ServeConn(server)
		_ = vmproto.WriteLine(client, guestproto.Request{
			V:             guestproto.Version,
			Op:            guestproto.OpContainerCopyFrom,
			ContainerCopy: &guestproto.ContainerCopyRequest{Path: "/hello.txt"},
		})
		br := bufio.NewReader(client)
		var r guestproto.Response
		if err := vmproto.ReadLine(br, &r); err != nil || !r.OK || r.PathStat == nil {
			t.Fatalf("copy-from resp = (%+v, %v)", r, err)
		}
		var tarData bytes.Buffer
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				t.Fatalf("copy-from frame: %v", err)
			}
			if kind == guestproto.FrameData {
				tarData.Write(payload)
			} else if kind == guestproto.FrameEOF {
				if len(payload) > 0 {
					t.Fatalf("copy-from eof error: %s", payload)
				}
				break
			}
		}
		_ = client.Close()
		if names := listTarNames(t, tarData.Bytes()); !slices.Equal(names, []string{"hello.txt"}) {
			t.Fatalf("copy-from tar = %v", names)
		}
	}

	// CopyTo
	{
		var tarBuf bytes.Buffer
		tw := tar.NewWriter(&tarBuf)
		_ = tw.WriteHeader(&tar.Header{Name: "uploaded.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
		_, _ = tw.Write([]byte("abc"))
		_ = tw.Close()

		client, server := net.Pipe()
		go srv.ServeConn(server)
		_ = vmproto.WriteLine(client, guestproto.Request{
			V:             guestproto.Version,
			Op:            guestproto.OpContainerCopyTo,
			ContainerCopy: &guestproto.ContainerCopyRequest{Path: "/"},
		})
		br := bufio.NewReader(client)
		var r guestproto.Response
		if err := vmproto.ReadLine(br, &r); err != nil || !r.OK {
			t.Fatalf("copy-to resp = (%+v, %v)", r, err)
		}
		_ = guestproto.WriteFrame(client, guestproto.FrameData, tarBuf.Bytes())
		_ = guestproto.WriteFrame(client, guestproto.FrameEOF, nil)

		kind, payload, err := guestproto.ReadFrame(br)
		_ = client.Close()
		if err != nil || kind != guestproto.FrameEOF || len(payload) != 0 {
			t.Fatalf("copy-to final eof = (%d, %q, %v)", kind, payload, err)
		}
		got, err := os.ReadFile(filepath.Join(ctr.rootfsDir, "uploaded.txt"))
		if err != nil || string(got) != "abc" {
			t.Fatalf("uploaded.txt = (%q, %v)", got, err)
		}
	}

	// Export (upperOnly)
	{
		client, server := net.Pipe()
		go srv.ServeConn(server)
		_ = vmproto.WriteLine(client, guestproto.Request{
			V:               guestproto.Version,
			Op:              guestproto.OpContainerExport,
			ContainerExport: &guestproto.ContainerExportRequest{UpperOnly: true},
		})
		br := bufio.NewReader(client)
		var r guestproto.Response
		if err := vmproto.ReadLine(br, &r); err != nil || !r.OK {
			t.Fatalf("export resp = (%+v, %v)", r, err)
		}
		var tarData bytes.Buffer
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				t.Fatalf("export frame: %v", err)
			}
			if kind == guestproto.FrameData {
				tarData.Write(payload)
			} else if kind == guestproto.FrameEOF {
				if len(payload) > 0 {
					t.Fatalf("export eof error: %s", payload)
				}
				break
			}
		}
		_ = client.Close()
		if names := listTarNames(t, tarData.Bytes()); !slices.Contains(names, "uploaded.txt") {
			t.Fatalf("export tar = %v", names)
		}
	}

	// 7. Attach replays buffered output, accepts stdin + resize, and gets FrameExit on Shutdown.
	{
		client, server := net.Pipe()
		go srv.ServeConn(server)
		_ = vmproto.WriteLine(client, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerAttach})
		br := bufio.NewReader(client)
		var r guestproto.Response
		if err := vmproto.ReadLine(br, &r); err != nil || !r.OK || r.Container == nil || !r.Container.Running {
			t.Fatalf("attach resp = (%+v, %v)", r, err)
		}

		// Send stdin and resize over attach stream, then trigger Shutdown to exit container.
		_ = guestproto.WriteFrame(client, guestproto.FrameStdin, []byte("cmd\n"))
		resizePayload, _ := json.Marshal(guestproto.ContainerResizeRequest{Rows: 50, Cols: 160})
		_ = guestproto.WriteFrame(client, guestproto.FrameResize, resizePayload)

		// Shutdown stops the container cleanly before unmounting /var/lib/gvx.
		go func() {
			time.Sleep(25 * time.Millisecond)
			resp := roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpShutdown})
			if !resp.OK {
				t.Errorf("shutdown failed: %+v", resp)
			}
		}()

		var out bytes.Buffer
		var exit guestproto.Exit
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				t.Fatalf("attach frame: %v", err)
			}
			if kind == guestproto.FrameStdout {
				out.Write(payload)
			} else if kind == guestproto.FrameExit {
				_ = json.Unmarshal(payload, &exit)
				break
			}
		}
		_ = client.Close()

		if out.String() != "booted\n" || exit.Code != 0 {
			t.Fatalf("attach got out=%q exit=%+v", out.String(), exit)
		}
		if ctr.shutdownCalls.Load() != 1 {
			t.Fatalf("container Shutdown calls = %d, want 1", ctr.shutdownCalls.Load())
		}
		if ops.unmountCount.Load() != 1 {
			t.Fatalf("disk unmountCount = %d, want 1", ops.unmountCount.Load())
		}
	}
}
