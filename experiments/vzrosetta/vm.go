//go:build darwin && arm64 && cgo

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/ironpark/gvx-desktop-vm/experiments/vzrosetta/proto"
)

// multi is a flag given more than once.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

// The names the guest's init knows a share by. Rosetta's is the mount
// tag; the others are what -share names.
const rosettaTag = "rosetta"

// The abstract Unix socket rosettad serves translations on, with
// -rosetta-cache.
const rosettaSocket = "gvx-rosetta"

// clock tells the time since the VM was asked to start.
type clock struct{ start time.Time }

func (c clock) since() string {
	return fmt.Sprintf("+%.3fs", time.Since(c.start).Seconds())
}

func note(format string, a ...any) {
	fmt.Printf("vzrosetta: "+format+"\n", a...)
}

func runVM(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	kernel := fs.String("kernel", "/tmp/gvxd-vz/build/Image", "an uncompressed arm64 Linux Image")
	initrd := fs.String("initrd", "/tmp/gvxd-vz/build/initramfs.gz", "the initramfs mkinitramfs.py made")
	cpus := fs.Uint("cpus", 4, "virtual CPUs")
	memMiB := fs.Uint64("mem", 2048, "memory, MiB")
	rosetta := fs.Bool("rosetta", false, "share Rosetta for Linux with the guest, which registers it with binfmt_misc")
	rosettaCache := fs.Bool("rosetta-cache", false, "with -rosetta: let Rosetta keep its translations (rosettad in the guest, macOS 14)")
	network := fs.Bool("net", false, "give the guest a NAT network device")
	sock := fs.String("sock", "/tmp/gvxd-vz/agent.sock", "a Unix socket to forward to the agent in the guest")
	verbose := fs.Bool("verbose", false, "let the kernel log to the console")
	extra := fs.String("cmdline", "", "more of a kernel command line")
	var shares, forwards multi
	fs.Var(&shares, "share", "tag=directory[:ro], shared over virtiofs and mounted at /mnt/tag; may be repeated")
	fs.Var(&forwards, "forward", "host.sock=guest.sock, a Unix socket of the guest made one of the host; may be repeated")
	fs.Parse(args)

	// What the host can do, before anything is asked of it.
	availability := vz.LinuxRosettaDirectoryShareAvailability()
	note("VZLinuxRosettaDirectoryShare.availability = %s", availability)

	cmdline := "console=hvc0 panic=-1"
	if !*verbose {
		cmdline += " quiet"
	}
	var tags []string
	var devices []vz.DirectorySharingDeviceConfiguration
	for _, s := range shares {
		tag, dir, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("-share %q: want tag=directory", s)
		}
		dir, readOnly := strings.CutSuffix(dir, ":ro")
		shared, err := vz.NewSharedDirectory(dir, readOnly)
		if err != nil {
			return fmt.Errorf("share %s: %w", tag, err)
		}
		single, err := vz.NewSingleDirectoryShare(shared)
		if err != nil {
			return err
		}
		dev, err := vz.NewVirtioFileSystemDeviceConfiguration(tag)
		if err != nil {
			return err
		}
		dev.SetDirectoryShare(single)
		devices = append(devices, dev)
		if readOnly {
			tag += ":ro"
		}
		tags = append(tags, tag)
	}
	if *rosetta {
		if availability != vz.LinuxRosettaAvailabilityInstalled {
			return fmt.Errorf("Rosetta for Linux is %s: softwareupdate --install-rosetta", availability)
		}
		share, err := vz.NewLinuxRosettaDirectoryShare()
		if err != nil {
			return fmt.Errorf("rosetta share: %w", err)
		}
		if *rosettaCache {
			// The translator asks rosettad for translations over this
			// socket; the guest's init starts rosettad, which listens on it.
			caching, err := vz.NewLinuxRosettaAbstractSocketCachingOptions(rosettaSocket)
			if err != nil {
				return fmt.Errorf("rosetta caching: %w", err)
			}
			share.SetOptions(caching)
			cmdline += " vz.rosettad=1"
		}
		dev, err := vz.NewVirtioFileSystemDeviceConfiguration(rosettaTag)
		if err != nil {
			return err
		}
		dev.SetDirectoryShare(share)
		devices = append(devices, dev)
		cmdline += " vz.rosetta=" + rosettaTag
	}
	if len(tags) > 0 {
		cmdline += " vz.shares=" + strings.Join(tags, ",")
	}
	if *network {
		cmdline += " vz.net=1"
	}
	if *extra != "" {
		cmdline += " " + *extra
	}

	bootLoader, err := vz.NewLinuxBootLoader(*kernel, vz.WithCommandLine(cmdline), vz.WithInitrd(*initrd))
	if err != nil {
		return fmt.Errorf("boot loader: %w", err)
	}
	config, err := vz.NewVirtualMachineConfiguration(bootLoader, *cpus, *memMiB<<20)
	if err != nil {
		return err
	}

	// The console: what the guest writes comes out of a pipe, a line at a
	// time, with the time since the start; it is given nothing to read.
	consoleIn, consoleInW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer consoleInW.Close()
	consoleOutR, consoleOut, err := os.Pipe()
	if err != nil {
		return err
	}
	attachment, err := vz.NewFileHandleSerialPortAttachment(consoleIn, consoleOut)
	if err != nil {
		return err
	}
	serial, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(attachment)
	if err != nil {
		return err
	}
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serial})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return err
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return err
	}
	config.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})

	vsock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return err
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsock})

	if *network {
		nat, err := vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return err
		}
		nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
		if err != nil {
			return err
		}
		mac, err := vz.NewRandomLocallyAdministeredMACAddress()
		if err != nil {
			return err
		}
		nic.SetMACAddress(mac)
		config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})
	}
	if len(devices) > 0 {
		config.SetDirectorySharingDevicesVirtualMachineConfiguration(devices)
	}
	if ok, err := config.Validate(); !ok || err != nil {
		return fmt.Errorf("the configuration is not valid: %w", err)
	}

	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		return err
	}

	// The guest runs in an XPC service of Virtualization.framework, not in
	// this process: it is the one whose memory is the VM's.
	before := vmProcesses()
	note("kernel command line: %s", cmdline)
	t := clock{time.Now()}
	go console(consoleOutR, t)
	if err := vm.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	note("%s VZVirtualMachine.start returned", t.since())
	var vmPID string
	for pid := range vmProcesses() {
		if !before[pid] {
			vmPID = pid
		}
	}
	note("pid %d, VM process (com.apple.Virtualization.VirtualMachine) pid %s", os.Getpid(), vmPID)

	device := vm.SocketDevices()[0]
	dialAgent := func() (net.Conn, error) { return device.Connect(proto.AgentPort) }

	// The agent listens once the guest's init has started it.
	go func() {
		for {
			conn, err := dialAgent()
			if err == nil {
				conn.Close()
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		note("%s the agent accepts on vsock port %d", t.since(), proto.AgentPort)
		note("RSS now: this process %s, VM process %s", rss(fmt.Sprint(os.Getpid())), rss(vmPID))
	}()

	if err := forward(*sock, "", dialAgent); err != nil {
		return err
	}
	defer os.Remove(*sock)
	for _, f := range forwards {
		host, guest, ok := strings.Cut(f, "=")
		if !ok {
			return fmt.Errorf("-forward %q: want host.sock=guest.sock", f)
		}
		if err := forward(host, guest, dialAgent); err != nil {
			return err
		}
		defer os.Remove(host)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-signals:
			note("%s stopping the VM", t.since())
			if vm.CanStop() {
				if err := vm.Stop(); err != nil {
					return fmt.Errorf("stop: %w", err)
				}
			}
		case state := <-vm.StateChangedNotify():
			note("%s state: %s", t.since(), state)
			switch state {
			case vz.VirtualMachineStateStopped:
				return nil
			case vz.VirtualMachineStateError:
				return fmt.Errorf("the VM stopped on an error")
			}
		}
	}
}

// console copies what the guest writes to stdout, each line after the
// time it came at.
func console(r io.Reader, t clock) {
	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		fmt.Printf("[%s] %s\n", t.since(), strings.TrimRight(lines.Text(), "\r"))
	}
	if err := lines.Err(); err != nil {
		note("the console: %v", err)
	}
}

// forward listens on a Unix socket of the host and joins each connection
// to one with the agent: as it is when guest is empty, so that a client
// speaks the agent's protocol itself, and otherwise to the Unix socket
// guest of the guest.
func forward(host, guest string, dialAgent func() (net.Conn, error)) error {
	os.Remove(host)
	l, err := net.Listen("unix", host)
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				agent, err := dialAgent()
				if err != nil {
					note("forward %s: the agent: %v", host, err)
					return
				}
				defer agent.Close()
				var fromAgent io.Reader = agent
				if guest != "" {
					if err := proto.WriteRequest(agent, proto.Request{Op: proto.OpDial, Addr: guest}); err != nil {
						return
					}
					r := bufio.NewReader(agent)
					if line, err := r.ReadString('\n'); err != nil || line != "ok\n" {
						note("forward %s: %s%v", host, line, err)
						return
					}
					fromAgent = r
				}
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					io.Copy(conn, fromAgent)
					conn.(*net.UnixConn).CloseWrite()
				}()
				io.Copy(agent, conn)
				// VZVirtioSocketConnection has no half close: when the
				// client is done writing, the connection is done.
				agent.Close()
				wg.Wait()
			}()
		}
	}()
	return nil
}

// vmProcesses is the set of pids of Virtualization.framework's VM
// services now running, this VM's or another program's.
func vmProcesses() map[string]bool {
	out, _ := exec.Command("/usr/bin/pgrep", "-f", "com.apple.Virtualization.VirtualMachine").Output()
	pids := map[string]bool{}
	for _, pid := range strings.Fields(string(out)) {
		pids[pid] = true
	}
	return pids
}

// rss is the resident size of a process as ps tells it.
func rss(pid string) string {
	if pid == "" {
		return "?"
	}
	out, err := exec.Command("/bin/ps", "-o", "rss=", "-p", pid).Output()
	if err != nil {
		return "?"
	}
	var kb int
	fmt.Sscan(string(out), &kb)
	return fmt.Sprintf("%.1f MiB", float64(kb)/1024)
}
