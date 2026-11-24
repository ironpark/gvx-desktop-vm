package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
)

const (
	ctrInitArg   = "__ctr-init"
	ctrRootfs    = "/mnt/rootfs"
	ctrRunDir    = "/run/gvx-ctr"
	ctrCgroupDir = "/sys/fs/cgroup/gvx-ctr"
	ctrUpperDir  = "/var/lib/gvx/upper"
	ctrWorkDir   = "/var/lib/gvx/work"
	ctrLayerBase = "/mnt/l"
	ctrVolBase   = "/mnt/vol"

	// defaultOutputBufBytes bounds buffered stdout/stderr kept for late attach.
	defaultOutputBufBytes = 4 << 20

	// whiteoutPrefix and opaqueMarker are the OCI/Docker layer tar conventions
	// corresponding to Linux overlayfs char(0,0) whiteouts and opaque dirs.
	ociWhiteoutPrefix = ".wh."
	ociOpaqueMarker   = ".wh..wh..opq"
)

// codedError wraps an error with a guestproto error code so server handlers
// return accurate machine-readable codes (e.g. CodeNotRunning, CodeNotFound).
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.err.Error()
}

func (e *codedError) Unwrap() error { return e.err }

func errWithCode(code string, err error) error {
	return &codedError{code: code, err: err}
}

func errorCodeOf(err error) string {
	var ce *codedError
	if errors.As(err, &ce) && ce.code != "" {
		return ce.code
	}
	return guestproto.CodeFailed
}

// ctrInitConfig is passed from gvx-agent to its __ctr-init child process over
// an inherited file descriptor before pivot_root and unix.Exec.
type ctrInitConfig struct {
	Argv           []string     `json:"argv"`
	Env            []string     `json:"env"`
	Workdir        string       `json:"workdir"`
	Hostname       string       `json:"hostname"`
	Privileged     bool         `json:"privileged"`
	ReadonlyRootfs bool         `json:"readonlyRootfs"`
	User           resolvedUser `json:"user"`
}

// containerOps abstracts container runtime actions so Server can be tested on
// any OS with a fake backend.
type containerOps interface {
	Start(spec guestproto.ContainerSpec) (guestproto.ContainerState, error)
	Attach() (guestproto.ContainerState, *outputHub, func([]byte) error, func() error, func(rows, cols uint16) error, error)
	Signal(req guestproto.ContainerSignalRequest) error
	Resize(rows, cols uint16) error
	Exec(req guestproto.ContainerExecRequest) (containerExecProc, error)
	Top() ([]guestproto.ContainerProcess, error)
	Stats() (guestproto.ContainerStats, error)
	Pause() error
	Unpause() error
	Stat(req guestproto.ContainerCopyRequest) (guestproto.ContainerPathStat, error)
	CopyFrom(req guestproto.ContainerCopyRequest) (guestproto.ContainerPathStat, func(w io.Writer) error, error)
	CopyTo(req guestproto.ContainerCopyRequest) (func(r io.Reader) error, error)
	Export(req guestproto.ContainerExportRequest) (func(w io.Writer) error, error)
	Shutdown(timeout time.Duration) error
}

// containerExecProc represents one running OpContainerExec process.
type containerExecProc interface {
	PID() int
	WriteStdin(p []byte) error
	CloseStdin() error
	Resize(rows, cols uint16) error
	StreamOutput(writeFrame func(kind byte, p []byte) error)
	Wait() guestproto.Exit
	Close()
}

// outputChunk is a single tagged stdout or stderr chunk in outputHub.
type outputChunk struct {
	kind byte
	data []byte
}

// outputHub buffers container stdout/stderr up to maxBytes and broadcasts live
// output and final exit to any number of concurrent or late attach subscribers.
type outputHub struct {
	mu       sync.Mutex
	chunks   []outputChunk
	baseSeq  int64
	totalB   int
	maxBytes int
	exited   bool
	exit     guestproto.Exit
	notify   chan struct{}
}

func newOutputHub(maxBytes int) *outputHub {
	if maxBytes <= 0 {
		maxBytes = defaultOutputBufBytes
	}
	return &outputHub{
		maxBytes: maxBytes,
		notify:   make(chan struct{}),
	}
}

func (h *outputHub) Append(kind byte, p []byte) {
	if len(p) == 0 {
		return
	}
	cp := make([]byte, len(p))
	copy(cp, p)

	h.mu.Lock()
	h.chunks = append(h.chunks, outputChunk{kind: kind, data: cp})
	h.totalB += len(cp)
	for h.totalB > h.maxBytes && len(h.chunks) > 1 {
		h.totalB -= len(h.chunks[0].data)
		h.chunks[0].data = nil
		h.chunks = h.chunks[1:]
		h.baseSeq++
	}
	h.wakeLocked()
	h.mu.Unlock()
}

func (h *outputHub) Finish(exit guestproto.Exit) {
	h.mu.Lock()
	if !h.exited {
		h.exited = true
		h.exit = exit
		h.wakeLocked()
	}
	h.mu.Unlock()
}

func (h *outputHub) wakeLocked() {
	close(h.notify)
	h.notify = make(chan struct{})
}

// Stream replays buffered chunks and streams live chunks to send until the
// container exits (returning exit, true) or stopCh is closed (returning zero, false).
func (h *outputHub) Stream(stopCh <-chan struct{}, send func(kind byte, p []byte) error) (guestproto.Exit, bool, error) {
	var seq int64
	for {
		h.mu.Lock()
		if seq < h.baseSeq {
			seq = h.baseSeq
		}
		idx := int(seq - h.baseSeq)
		if idx < len(h.chunks) {
			batch := make([]outputChunk, len(h.chunks)-idx)
			copy(batch, h.chunks[idx:])
			seq += int64(len(batch))
			h.mu.Unlock()
			for _, ch := range batch {
				if err := send(ch.kind, ch.data); err != nil {
					return guestproto.Exit{}, false, err
				}
			}
			continue
		}
		if h.exited {
			ex := h.exit
			h.mu.Unlock()
			return ex, true, nil
		}
		waitCh := h.notify
		h.mu.Unlock()

		select {
		case <-waitCh:
		case <-stopCh:
			return guestproto.Exit{}, false, nil
		}
	}
}

// buildLowerDir assembles an overlayfs lowerdir option string from bottom-to-top
// layer mount points. OverlayFS expects topmost layer first (leftmost).
func buildLowerDir(bottomToTop []string) string {
	if len(bottomToTop) == 0 {
		return ""
	}
	rev := make([]string, len(bottomToTop))
	for i := range bottomToTop {
		rev[i] = bottomToTop[len(bottomToTop)-1-i]
	}
	return strings.Join(rev, ":")
}

// validateContainerSpec checks that a ContainerSpec is well-formed before any
// mounts or processes are created.
func validateContainerSpec(spec *guestproto.ContainerSpec) error {
	if spec == nil {
		return errors.New("container spec is required")
	}
	if len(spec.Argv) == 0 || strings.TrimSpace(spec.Argv[0]) == "" {
		return errors.New("container argv is required")
	}
	if len(spec.Layers) == 0 {
		return errors.New("container requires at least one layer")
	}
	for i, l := range spec.Layers {
		hasDev := strings.TrimSpace(l.Device) != ""
		hasFile := strings.TrimSpace(l.File) != ""
		if hasDev == hasFile {
			return fmt.Errorf("layer %d must set exactly one of device or file", i)
		}
		if hasDev && !strings.HasPrefix(l.Device, "/dev/") {
			return fmt.Errorf("layer %d device %q must be under /dev/", i, l.Device)
		}
		if hasFile && !filepath.IsAbs(l.File) {
			return fmt.Errorf("layer %d file %q must be an absolute path", i, l.File)
		}
	}
	for i, m := range spec.Mounts {
		switch m.Kind {
		case "bind", "volume":
			if strings.TrimSpace(m.Source) == "" || !filepath.IsAbs(m.Source) {
				return fmt.Errorf("mount %d (%s) requires an absolute source path", i, m.Kind)
			}
		case "tmpfs":
		default:
			return fmt.Errorf("mount %d has unsupported kind %q", i, m.Kind)
		}
		if strings.TrimSpace(m.Target) == "" || !strings.HasPrefix(m.Target, "/") {
			return fmt.Errorf("mount %d target %q must be an absolute container path", i, m.Target)
		}
		clean := path.Clean(m.Target)
		if clean == "/" {
			return fmt.Errorf("mount %d target cannot be /", i)
		}
	}
	return nil
}

// resolveRootfsPath resolves containerPath inside rootfs without ever escaping
// rootfs, even in the presence of ".." segments or absolute/relative symlinks.
// If followLast is true, a trailing symlink is also resolved within rootfs.
func resolveRootfsPath(rootfs, containerPath string, followLast bool) (string, error) {
	rootAbs, err := filepath.Abs(rootfs)
	if err != nil {
		return "", err
	}
	cur := "/"
	parts := splitContainerPath(containerPath)
	const maxSymlinks = 40
	links := 0

	for len(parts) > 0 {
		seg := parts[0]
		parts = parts[1:]
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			cur = path.Dir(cur)
			continue
		}

		next := path.Join(cur, seg)
		hostNext := filepath.Join(rootAbs, filepath.FromSlash(strings.TrimPrefix(next, "/")))
		isLast := len(parts) == 0

		fi, err := os.Lstat(hostNext)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Keep walking remaining lexical segments so callers like
				// MkdirAll/CopyTo can create missing leaf paths safely.
				cur = next
				continue
			}
			return "", err
		}

		if fi.Mode()&os.ModeSymlink != 0 && (!isLast || followLast) {
			links++
			if links > maxSymlinks {
				return "", fmt.Errorf("too many levels of symbolic links resolving %q", containerPath)
			}
			target, err := os.Readlink(hostNext)
			if err != nil {
				return "", err
			}
			var expanded string
			if strings.HasPrefix(target, "/") {
				expanded = target
				cur = "/"
			} else {
				expanded = target
			}
			newParts := splitContainerPath(expanded)
			parts = append(newParts, parts...)
			continue
		}

		cur = next
	}

	resolved := filepath.Join(rootAbs, filepath.FromSlash(strings.TrimPrefix(cur, "/")))
	if resolved != rootAbs && !strings.HasPrefix(resolved, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes container rootfs", containerPath)
	}
	return resolved, nil
}

func splitContainerPath(p string) []string {
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// linuxSignalMap maps canonical Linux signal names (without "SIG" prefix) to
// Linux signal numbers (identical on arm64 and amd64 Linux).
var linuxSignalMap = map[string]int{
	"HUP":    1,
	"INT":    2,
	"QUIT":   3,
	"ILL":    4,
	"TRAP":   5,
	"ABRT":   6,
	"IOT":    6,
	"BUS":    7,
	"FPE":    8,
	"KILL":   9,
	"USR1":   10,
	"SEGV":   11,
	"USR2":   12,
	"PIPE":   13,
	"ALRM":   14,
	"TERM":   15,
	"STKFLT": 16,
	"CHLD":   17,
	"CLD":    17,
	"CONT":   18,
	"STOP":   19,
	"TSTP":   20,
	"TTIN":   21,
	"TTOU":   22,
	"URG":    23,
	"XCPU":   24,
	"XFSZ":   25,
	"VTALRM": 26,
	"PROF":   27,
	"WINCH":  28,
	"IO":     29,
	"POLL":   29,
	"PWR":    30,
	"SYS":    31,
}

var linuxSignalNames = func() map[int]string {
	m := make(map[int]string, len(linuxSignalMap))
	for name, num := range linuxSignalMap {
		if name == "IOT" || name == "CLD" || name == "POLL" {
			continue
		}
		m[num] = "SIG" + name
	}
	return m
}()

// parseSignalName parses a Docker signal name ("SIGTERM", "TERM", "9", "") into
// its Linux signal number and canonical "SIG..." name. Empty defaults to SIGKILL.
func parseSignalName(spec string) (int, string, error) {
	s := strings.ToUpper(strings.TrimSpace(spec))
	if s == "" {
		return 9, "SIGKILL", nil
	}
	if num, err := strconv.Atoi(s); err == nil {
		if num < 1 || num > 64 {
			return 0, "", fmt.Errorf("signal number %d out of range [1, 64]", num)
		}
		if name, ok := linuxSignalNames[num]; ok {
			return num, name, nil
		}
		return num, fmt.Sprintf("SIGRT%d", num), nil
	}
	s = strings.TrimPrefix(s, "SIG")
	num, ok := linuxSignalMap[s]
	if !ok {
		return 0, "", fmt.Errorf("unknown signal %q", spec)
	}
	return num, linuxSignalNames[num], nil
}

// renderHostsFile constructs /etc/hosts for a container, including localhost,
// container hostname, dynamic host.docker.internal gateway mapping, and
// ExtraHosts entries (resolving "host-gateway" to gatewayIP).
func renderHostsFile(hostname, gatewayIP string, extraHosts []string) string {
	var sb strings.Builder
	sb.WriteString("127.0.0.1\tlocalhost\n")
	sb.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")

	hostname = sanitizeHostToken(hostname)
	if hostname != "" && hostname != "localhost" {
		fmt.Fprintf(&sb, "127.0.0.1\t%s\n", hostname)
	}

	gatewayIP = sanitizeHostToken(gatewayIP)
	hasDockerInternal := false
	type extraEntry struct{ host, ip string }
	var extras []extraEntry

	for _, raw := range extraHosts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Docker ExtraHosts format is "hostname:ip" (or "hostname:host-gateway").
		// Use IndexByte so IPv6 addresses after the first ':' are preserved.
		idx := strings.IndexByte(raw, ':')
		if idx <= 0 || idx == len(raw)-1 {
			continue
		}
		host := sanitizeHostToken(raw[:idx])
		ip := sanitizeHostToken(raw[idx+1:])
		if host == "" || ip == "" {
			continue
		}
		if ip == "host-gateway" {
			if gatewayIP == "" {
				continue
			}
			ip = gatewayIP
		}
		if host == "host.docker.internal" {
			hasDockerInternal = true
		}
		extras = append(extras, extraEntry{host: host, ip: ip})
	}

	if !hasDockerInternal && gatewayIP != "" {
		fmt.Fprintf(&sb, "%s\thost.docker.internal\n", gatewayIP)
	}
	for _, e := range extras {
		fmt.Fprintf(&sb, "%s\t%s\n", e.ip, e.host)
	}
	return sb.String()
}

func sanitizeHostToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	return s
}

// parseDefaultGateway extracts the IPv4 default gateway address from the
// contents of Linux's /proc/net/route.
func parseDefaultGateway(procNetRoute string) string {
	sc := bufio.NewScanner(strings.NewReader(procNetRoute))
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		// Column 1 is Destination ("00000000" for default route), column 2 is
		// Gateway in little-endian 32-bit hex, column 3 is Flags (bit 1 = RTF_UP, bit 2 = RTF_GATEWAY).
		if fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x3 != 0x3 {
			continue
		}
		gw, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil || gw == 0 {
			continue
		}
		return fmt.Sprintf("%d.%d.%d.%d", byte(gw), byte(gw>>8), byte(gw>>16), byte(gw>>24))
	}
	return ""
}

// parseCgroupStats parses cgroup v2 files and /proc/net/dev into ContainerStats.
func parseCgroupStats(cpuStat, memCur, memStat, memMax, memInfo, pidsCur, pidsMax, netDev string) guestproto.ContainerStats {
	st := guestproto.ContainerStats{
		Timestamp: time.Now().UTC(),
	}

	for _, line := range strings.Split(cpuStat, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "user_usec":
			st.CPUUserNanos = int64(v) * 1000
		case "system_usec":
			st.CPUSystemNanos = int64(v) * 1000
		case "nr_periods":
			st.CPUPeriods = v
		case "nr_throttled":
			st.CPUThrottled = v
		case "throttled_usec":
			st.CPUThrottledNs = int64(v) * 1000
		}
	}

	if v, err := strconv.ParseUint(strings.TrimSpace(memCur), 10, 64); err == nil {
		st.MemoryBytes = v
	}
	for _, line := range strings.Split(memStat, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "anon":
			st.MemoryAnonBytes = v
		case "file":
			st.MemoryFileBytes = v
		case "shmem":
			st.MemoryTmpfsBytes = v
		}
	}

	memMaxTrim := strings.TrimSpace(memMax)
	if memMaxTrim != "" && memMaxTrim != "max" {
		if v, err := strconv.ParseUint(memMaxTrim, 10, 64); err == nil {
			st.MemoryLimitBytes = v
		}
	}
	if st.MemoryLimitBytes == 0 && memInfo != "" {
		for _, line := range strings.Split(memInfo, "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						st.MemoryLimitBytes = kb * 1024
					}
				}
				break
			}
		}
	}

	if v, err := strconv.Atoi(strings.TrimSpace(pidsCur)); err == nil {
		st.PIDs = v
	}
	pidsMaxTrim := strings.TrimSpace(pidsMax)
	if pidsMaxTrim != "" && pidsMaxTrim != "max" {
		if v, err := strconv.ParseInt(pidsMaxTrim, 10, 64); err == nil {
			st.PIDsLimit = v
		}
	}

	st.Network = parseProcNetDev(netDev)
	return st
}

func parseProcNetDev(netDev string) []guestproto.ContainerNetStats {
	var out []guestproto.ContainerNetStats
	for _, line := range strings.Split(netDev, "\n") {
		iface, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		iface = strings.TrimSpace(iface)
		if iface == "" || iface == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 10 {
			continue
		}
		rxBytes, _ := strconv.ParseUint(fields[0], 10, 64)
		rxPkts, _ := strconv.ParseUint(fields[1], 10, 64)
		txBytes, _ := strconv.ParseUint(fields[8], 10, 64)
		txPkts, _ := strconv.ParseUint(fields[9], 10, 64)
		out = append(out, guestproto.ContainerNetStats{
			Interface: iface,
			RxBytes:   rxBytes,
			TxBytes:   txBytes,
			RxPackets: rxPkts,
			TxPackets: txPkts,
		})
	}
	return out
}

// parseProcProcess parses /proc/<pid>/{stat,status,cmdline} into a ContainerProcess.
func parseProcProcess(pid int32, statData, statusData string, cmdlineData []byte, clkTck int64, pageSize uint64) (guestproto.ContainerProcess, bool) {
	openParen := strings.IndexByte(statData, '(')
	closeParen := strings.LastIndexByte(statData, ')')
	if openParen < 0 || closeParen <= openParen || closeParen+2 > len(statData) {
		return guestproto.ContainerProcess{}, false
	}
	comm := statData[openParen+1 : closeParen]
	rest := strings.Fields(statData[closeParen+1:])
	// In /proc/<pid>/stat after "(comm) ":
	// rest[0]=state, rest[1]=ppid, rest[11]=utime, rest[12]=stime,
	// rest[17]=num_threads, rest[21]=rss
	if len(rest) < 22 {
		return guestproto.ContainerProcess{}, false
	}
	ppid, _ := strconv.ParseInt(rest[1], 10, 32)
	utime, _ := strconv.ParseInt(rest[11], 10, 64)
	stime, _ := strconv.ParseInt(rest[12], 10, 64)
	threads, _ := strconv.Atoi(rest[17])
	rssPages, _ := strconv.ParseUint(rest[21], 10, 64)

	if clkTck <= 0 {
		clkTck = 100
	}
	if pageSize == 0 {
		pageSize = 4096
	}

	proc := guestproto.ContainerProcess{
		PID:      pid,
		PPID:     int32(ppid),
		State:    rest[0],
		Threads:  threads,
		CPUNanos: (utime + stime) * (int64(time.Second) / clkTck),
		RSSBytes: rssPages * pageSize,
		Comm:     comm,
	}

	for _, line := range strings.Split(statusData, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Uid":
			// Real, effective, saved, fs UID — use effective UID if present.
			idx := 0
			if len(fields) > 1 {
				idx = 1
			}
			if u, err := strconv.ParseUint(fields[idx], 10, 32); err == nil {
				proc.UID = uint32(u)
			}
		case "Gid":
			idx := 0
			if len(fields) > 1 {
				idx = 1
			}
			if g, err := strconv.ParseUint(fields[idx], 10, 32); err == nil {
				proc.GID = uint32(g)
			}
		case "Threads":
			if t, err := strconv.Atoi(fields[0]); err == nil {
				proc.Threads = t
			}
		}
	}

	if len(cmdlineData) > 0 {
		trimmed := bytes.TrimRight(cmdlineData, "\x00")
		if len(trimmed) > 0 {
			parts := bytes.Split(trimmed, []byte{0})
			proc.Cmdline = make([]string, len(parts))
			for i, p := range parts {
				proc.Cmdline[i] = string(p)
			}
		}
	}
	return proc, true
}

// mergeEnv overlays overrides onto base (replacing matching KEY= entries while
// preserving order) and ensures default PATH is present.
func mergeEnv(base, overrides []string, defaultHome string, tty bool) []string {
	type kv struct{ key, entry string }
	var items []kv
	index := make(map[string]int)

	add := func(entries []string) {
		for _, e := range entries {
			k, _, ok := strings.Cut(e, "=")
			if !ok || k == "" {
				continue
			}
			if i, exists := index[k]; exists {
				items[i].entry = e
			} else {
				index[k] = len(items)
				items = append(items, kv{key: k, entry: e})
			}
		}
	}
	add(base)
	add(overrides)

	if _, ok := index["PATH"]; !ok {
		add([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"})
	}
	if _, ok := index["HOME"]; !ok && defaultHome != "" {
		add([]string{"HOME=" + defaultHome})
	}
	if tty {
		if _, ok := index["TERM"]; !ok {
			add([]string{"TERM=xterm"})
		}
	}

	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.entry
	}
	return out
}

// statContainerPath inspects containerPath inside rootfs and returns a
// ContainerPathStat matching Docker archive/stat conventions.
func statContainerPath(rootfs, containerPath string, followLink bool) (guestproto.ContainerPathStat, string, error) {
	resolved, err := resolveRootfsPath(rootfs, containerPath, followLink)
	if err != nil {
		return guestproto.ContainerPathStat{}, "", errWithCode(guestproto.CodeBadRequest, err)
	}
	fi, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return guestproto.ContainerPathStat{}, "", errWithCode(guestproto.CodeNotFound, err)
		}
		return guestproto.ContainerPathStat{}, "", err
	}
	name := path.Base(path.Clean("/" + containerPath))
	if name == "." {
		name = "/"
	}
	st := guestproto.ContainerPathStat{
		Name:  name,
		Size:  fi.Size(),
		Mode:  uint32(fi.Mode()),
		Mtime: fi.ModTime().UTC(),
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if link, err := os.Readlink(resolved); err == nil {
			st.LinkTarget = link
		}
	}
	return st, resolved, nil
}

// writeTarFromPath streams a tar archive of hostPath into w.
//
// Modes:
//   - includeBase=true (CopyFrom): a file or directory is archived under its
//     basename (e.g. "foo" or "dir/..."), matching `docker cp`.
//   - includeBase=false (Export): children of hostPath are archived directly
//     relative to hostPath (no leading "/").
//   - upperOnly=true: translates Linux overlayfs char(0,0) whiteouts into
//     ".wh.<name>" and opaque directories into ".wh..wh..opq".
//   - skipVirtual=true: when exporting full rootfs "/", keeps empty /proc, /sys,
//     /dev directories without descending into virtual filesystems.
func writeTarFromPath(hostPath string, includeBase, upperOnly, skipVirtual bool, w io.Writer) error {
	tw := tar.NewWriter(w)

	fi, err := os.Lstat(hostPath)
	if err != nil {
		if upperOnly && errors.Is(err, os.ErrNotExist) {
			return tw.Close()
		}
		return err
	}

	if !fi.IsDir() {
		name := filepath.Base(hostPath)
		if err := writeTarEntry(tw, hostPath, name, fi, upperOnly); err != nil {
			return err
		}
		return tw.Close()
	}

	baseName := filepath.Base(hostPath)
	if includeBase && (baseName == "" || baseName == "/" || baseName == ".") {
		includeBase = false
	}

	err = filepath.WalkDir(hostPath, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		rel, err := filepath.Rel(hostPath, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		var tarName string
		if rel == "." {
			if !includeBase {
				return nil
			}
			tarName = baseName
		} else if includeBase {
			tarName = baseName + "/" + rel
		} else {
			tarName = rel
		}

		info, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}

		if skipVirtual && info.IsDir() && (rel == "proc" || rel == "sys" || rel == "dev") {
			if err := writeTarEntry(tw, p, tarName, info, false); err != nil {
				return err
			}
			return filepath.SkipDir
		}

		if err := writeTarEntry(tw, p, tarName, info, upperOnly); err != nil {
			return err
		}
		if upperOnly && info.IsDir() && isOverlayOpaqueDir(p) {
			opqName := path.Join(tarName, ociOpaqueMarker)
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     opqName,
				Mode:     0o644,
				Size:     0,
				ModTime:  info.ModTime().UTC(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func writeTarEntry(tw *tar.Writer, hostPath, tarName string, info fs.FileInfo, upperOnly bool) error {
	if upperOnly && isOverlayWhiteout(info) {
		dir, base := path.Split(tarName)
		whName := dir + ociWhiteoutPrefix + base
		return tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     whName,
			Mode:     0o644,
			Size:     0,
			ModTime:  info.ModTime().UTC(),
		})
	}

	var link string
	if info.Mode()&os.ModeSymlink != 0 {
		var err error
		link, err = os.Readlink(hostPath)
		if err != nil {
			return err
		}
	}
	// Skip Unix domain sockets (tar cannot archive sockets cleanly).
	if info.Mode()&os.ModeSocket != 0 {
		return nil
	}

	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = tarName
	if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
		hdr.Name += "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(hostPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// cleanTarRelPath validates a tar entry or hardlink path and returns its
// cleaned relative path. It rejects NUL bytes, absolute paths, and any ".."
// traversal escaping the destination directory. Returns "" for ".".
func cleanTarRelPath(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("tar entry %q contains NUL byte", raw)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("tar entry %q must be relative", raw)
	}
	clean := path.Clean(raw)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("tar entry %q escapes destination directory", raw)
	}
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

func isWithinDir(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+string(filepath.Separator))
}

// extractTarToDir safely extracts a tar stream into destDir (which must be an
// existing directory inside rootfs), rejecting path traversal, escapes outside
// destDir via symlinks or hardlinks, and honoring noOverwriteDir.
func extractTarToDir(rootfs, destDir string, noOverwriteDir bool, r io.Reader) error {
	rootAbs, err := filepath.Abs(rootfs)
	if err != nil {
		return err
	}
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	if !isWithinDir(destAbs, rootAbs) {
		return fmt.Errorf("destination %q escapes rootfs", destDir)
	}
	relInRoot, err := filepath.Rel(rootAbs, destAbs)
	if err != nil {
		return err
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		cleanName, err := cleanTarRelPath(hdr.Name)
		if err != nil {
			return err
		}
		if cleanName == "" {
			continue
		}

		containerTarget := "/" + path.Join(filepath.ToSlash(relInRoot), cleanName)
		if relInRoot == "." {
			containerTarget = "/" + cleanName
		}

		target, err := resolveRootfsPath(rootAbs, containerTarget, false)
		if err != nil {
			return err
		}
		if !isWithinDir(target, destAbs) {
			return fmt.Errorf("tar entry %q escapes destination directory", hdr.Name)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		existing, statErr := os.Lstat(target)
		exists := statErr == nil

		switch hdr.Typeflag {
		case tar.TypeDir:
			if exists {
				if !existing.IsDir() {
					if noOverwriteDir {
						return fmt.Errorf("cannot overwrite non-directory %q with directory", cleanName)
					}
					if err := os.Remove(target); err != nil {
						return err
					}
					if err := os.Mkdir(target, fs.FileMode(hdr.Mode)&0o7777); err != nil {
						return err
					}
				}
			} else {
				if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)&0o7777); err != nil {
					return err
				}
			}
			_ = os.Chmod(target, fs.FileMode(hdr.Mode)&0o7777)
			if !hdr.ModTime.IsZero() {
				_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
			}

		case tar.TypeReg, tar.TypeRegA:
			if exists {
				if existing.IsDir() {
					if noOverwriteDir {
						return fmt.Errorf("cannot overwrite directory %q with non-directory", cleanName)
					}
					if err := os.RemoveAll(target); err != nil {
						return err
					}
				} else if err := os.Remove(target); err != nil {
					return err
				}
			}
			perm := fs.FileMode(hdr.Mode) & 0o7777
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			_ = os.Chmod(target, perm)
			if hdr.Uid != 0 || hdr.Gid != 0 {
				_ = os.Lchown(target, hdr.Uid, hdr.Gid)
			}
			if !hdr.ModTime.IsZero() {
				_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
			}

		case tar.TypeSymlink:
			if exists {
				if existing.IsDir() {
					if noOverwriteDir {
						return fmt.Errorf("cannot overwrite directory %q with symlink", cleanName)
					}
					if err := os.RemoveAll(target); err != nil {
						return err
					}
				} else if err := os.Remove(target); err != nil {
					return err
				}
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}

		case tar.TypeLink:
			linkClean, err := cleanTarRelPath(hdr.Linkname)
			if err != nil {
				return fmt.Errorf("hardlink %q -> %w", hdr.Name, err)
			}
			if linkClean == "" {
				return fmt.Errorf("hardlink %q has empty target", hdr.Name)
			}
			linkContainer := "/" + path.Join(filepath.ToSlash(relInRoot), linkClean)
			if relInRoot == "." {
				linkContainer = "/" + linkClean
			}
			linkHost, err := resolveRootfsPath(rootAbs, linkContainer, false)
			if err != nil {
				return err
			}
			if !isWithinDir(linkHost, destAbs) {
				return fmt.Errorf("hardlink %q target %q escapes destination directory", hdr.Name, hdr.Linkname)
			}
			if exists {
				if existing.IsDir() {
					if noOverwriteDir {
						return fmt.Errorf("cannot overwrite directory %q with hardlink", cleanName)
					}
					if err := os.RemoveAll(target); err != nil {
						return err
					}
				} else if err := os.Remove(target); err != nil {
					return err
				}
			}
			if err := os.Link(linkHost, target); err != nil {
				return err
			}

		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			// Handled transparently by archive/tar.
		}
	}
}

// signalWaitStatus extracts exit code and signal name from an exec wait result.
func extractExitInfo(ps *os.ProcessState, err error) (code int, sigName string, errMsg string) {
	if ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig := int(ws.Signal())
			name := linuxSignalNames[sig]
			if name == "" {
				name = ws.Signal().String()
			}
			return 128 + sig, name, ""
		}
		return ps.ExitCode(), "", ""
	}
	if err == nil {
		return 0, "", ""
	}
	return 127, "", err.Error()
}
