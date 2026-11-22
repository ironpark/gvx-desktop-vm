//go:build darwin

// Command vzpurego: can Virtualization.framework be driven from Go with
// CGO_ENABLED=0, through the Objective-C runtime (purego + purego/objc)?
// A throwaway: boots the vzrosetta prototype's kernel and initramfs with
// Rosetta, a share, a disk, NAT and vsock, pings the agent, runs commands,
// then stops the VM three ways.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// --- the runtime ----------------------------------------------------------

var (
	dispatch_queue_create func(label *byte, attr uintptr) uintptr
	dispatch_sync_f       func(q uintptr, ctx uintptr, fn uintptr)
	dispatch_async_f      func(q uintptr, ctx uintptr, fn uintptr)
	pool_push             func() uintptr
	pool_pop              func(uintptr)
	pthread_self          func() uintptr
	pthread_main_np       func() int32

	trampoline uintptr
	handles    sync.Map
	nextHandle atomic.Uintptr
)

func load() {
	if _, err := purego.Dlopen("/System/Library/Frameworks/Foundation.framework/Foundation", purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
		panic(err)
	}
	if _, err := purego.Dlopen("/System/Library/Frameworks/Virtualization.framework/Virtualization", purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
		panic(err)
	}
	libSystem, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		panic(err)
	}
	libobjc, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		panic(err)
	}
	purego.RegisterLibFunc(&dispatch_queue_create, libSystem, "dispatch_queue_create")
	purego.RegisterLibFunc(&dispatch_sync_f, libSystem, "dispatch_sync_f")
	purego.RegisterLibFunc(&dispatch_async_f, libSystem, "dispatch_async_f")
	purego.RegisterLibFunc(&pthread_self, libSystem, "pthread_self")
	purego.RegisterLibFunc(&pthread_main_np, libSystem, "pthread_main_np")
	purego.RegisterLibFunc(&pool_push, libobjc, "objc_autoreleasePoolPush")
	purego.RegisterLibFunc(&pool_pop, libobjc, "objc_autoreleasePoolPop")
	// One C callback for every hop onto a queue: purego callbacks are a
	// fixed table (2000) never freed, so the Go func rides in ctx.
	trampoline = purego.NewCallback(func(ctx uintptr) {
		f, _ := handles.LoadAndDelete(ctx)
		f.(func())()
	})
}

// onQueue runs f on q and waits.
func onQueue(q uintptr, f func()) {
	h := nextHandle.Add(1)
	handles.Store(h, func() {
		p := pool_push()
		defer pool_pop(p)
		f()
	})
	dispatch_sync_f(q, h, trampoline)
}

func sel(s string) objc.SEL { return objc.RegisterName(s) }
func class(s string) objc.ID {
	c := objc.GetClass(s)
	if c == 0 {
		panic("no class " + s)
	}
	return objc.ID(c)
}
func alloc(s string) objc.ID { return class(s).Send(sel("alloc")) }

func cstr(s string) *byte { b := append([]byte(s), 0); return &b[0] }

func gostr(p *byte) string {
	if p == nil {
		return ""
	}
	var n int
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// utf8 is an NSString's text.
func utf8(s objc.ID) string { return gostr(objc.Send[*byte](s, sel("UTF8String"))) }

func nsstr(s string) objc.ID {
	return alloc("NSString").Send(sel("initWithUTF8String:"), cstr(s))
}

func nsurl(path string) objc.ID {
	return alloc("NSURL").Send(sel("initFileURLWithPath:"), nsstr(path))
}

func nsarray(items ...objc.ID) objc.ID {
	if len(items) == 0 {
		return class("NSArray").Send(sel("array"))
	}
	return alloc("NSArray").Send(sel("initWithObjects:count:"), unsafe.Pointer(&items[0]), uint64(len(items)))
}

func nserr(e objc.ID) error {
	if e == 0 {
		return nil
	}
	desc := e.Send(sel("localizedDescription"))
	code := objc.Send[int64](e, sel("code"))
	return fmt.Errorf("%s (%s %d)", utf8(desc), utf8(e.Send(sel("domain"))), code)
}

func note(format string, a ...any) {
	fmt.Printf("[%s] spike: "+format+"\n", append([]any{time.Since(t0).Round(time.Millisecond)}, a...)...)
}

var t0 = time.Now()

// --- the VM ---------------------------------------------------------------

type vm struct {
	q       uintptr
	obj     objc.ID
	stopped chan string
}

func main() {
	work := flag.String("work", "/tmp/gvxd-vzs", "the prototype's work dir")
	flag.Parse()
	runtime.LockOSThread() // keep main on the main thread; we never use it for VZ
	load()
	note("pid %d, main thread? %v", os.Getpid(), pthread_main_np() == 1)

	avail := objc.Send[int64](class("VZLinuxRosettaDirectoryShare"), sel("availability"))
	note("VZLinuxRosettaDirectoryShare.availability = %d (2 = installed)", avail)

	q := dispatch_queue_create(cstr("gvx.vm"), 0)
	v := &vm{q: q, stopped: make(chan string, 4)}

	// Config objects may be built on any thread.
	p := pool_push()
	cmdline := "console=hvc0 panic=-1 quiet vz.rosetta=rosetta vz.shares=amd64:ro vz.net=1"
	boot := alloc("VZLinuxBootLoader").Send(sel("initWithKernelURL:"), nsurl(*work+"/build/Image"))
	boot.Send(sel("setInitialRamdiskURL:"), nsurl(*work+"/build/initramfs.gz"))
	boot.Send(sel("setCommandLine:"), nsstr(cmdline))

	cfg := alloc("VZVirtualMachineConfiguration").Send(sel("init"))
	cfg.Send(sel("setBootLoader:"), boot)
	cfg.Send(sel("setCPUCount:"), uint64(2))
	cfg.Send(sel("setMemorySize:"), uint64(2048<<20))

	// Console through pipes.
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	_ = inW
	rh := alloc("NSFileHandle").Send(sel("initWithFileDescriptor:"), int32(inR.Fd()))
	wh := alloc("NSFileHandle").Send(sel("initWithFileDescriptor:"), int32(outW.Fd()))
	att := alloc("VZFileHandleSerialPortAttachment").Send(sel("initWithFileHandleForReading:fileHandleForWriting:"), rh, wh)
	serial := alloc("VZVirtioConsoleDeviceSerialPortConfiguration").Send(sel("init"))
	serial.Send(sel("setAttachment:"), att)
	cfg.Send(sel("setSerialPorts:"), nsarray(serial))
	go func() {
		s := bufio.NewScanner(outR)
		for s.Scan() {
			fmt.Printf("[%s] console: %s\n", time.Since(t0).Round(time.Millisecond), s.Text())
		}
	}()

	cfg.Send(sel("setEntropyDevices:"), nsarray(alloc("VZVirtioEntropyDeviceConfiguration").Send(sel("init"))))
	cfg.Send(sel("setMemoryBalloonDevices:"), nsarray(alloc("VZVirtioTraditionalMemoryBalloonDeviceConfiguration").Send(sel("init"))))
	cfg.Send(sel("setSocketDevices:"), nsarray(alloc("VZVirtioSocketDeviceConfiguration").Send(sel("init"))))

	// NAT.
	nic := alloc("VZVirtioNetworkDeviceConfiguration").Send(sel("init"))
	nic.Send(sel("setAttachment:"), alloc("VZNATNetworkDeviceAttachment").Send(sel("init")))
	nic.Send(sel("setMACAddress:"), class("VZMACAddress").Send(sel("randomLocallyAdministeredAddress")))
	cfg.Send(sel("setNetworkDevices:"), nsarray(nic))

	// Rosetta, NSError** out-param.
	var nerr objc.ID
	ros := alloc("VZLinuxRosettaDirectoryShare").Send(sel("initWithError:"), unsafe.Pointer(&nerr))
	if ros == 0 {
		note("rosetta share: %v", nserr(nerr))
		os.Exit(1)
	}
	rdev := alloc("VZVirtioFileSystemDeviceConfiguration").Send(sel("initWithTag:"), nsstr("rosetta"))
	rdev.Send(sel("setShare:"), ros)
	shared := alloc("VZSharedDirectory").Send(sel("initWithURL:readOnly:"), nsurl(*work+"/amd64root"), true)
	single := alloc("VZSingleDirectoryShare").Send(sel("initWithDirectory:"), shared)
	sdev := alloc("VZVirtioFileSystemDeviceConfiguration").Send(sel("initWithTag:"), nsstr("amd64"))
	sdev.Send(sel("setShare:"), single)
	cfg.Send(sel("setDirectorySharingDevices:"), nsarray(rdev, sdev))

	// A sparse disk image, virtio-blk.
	disk := *work + "/spike-disk.img"
	f, _ := os.Create(disk)
	f.Truncate(256 << 20)
	f.Close()
	defer os.Remove(disk)
	nerr = 0
	datt := alloc("VZDiskImageStorageDeviceAttachment").Send(sel("initWithURL:readOnly:error:"), nsurl(disk), false, unsafe.Pointer(&nerr))
	if datt == 0 {
		note("disk: %v", nserr(nerr))
		os.Exit(1)
	}
	cfg.Send(sel("setStorageDevices:"), nsarray(alloc("VZVirtioBlockDeviceConfiguration").Send(sel("initWithAttachment:"), datt)))

	// validateWithError: — a BOOL return and an NSError** out-param.
	nerr = 0
	if !objc.Send[bool](cfg, sel("validateWithError:"), unsafe.Pointer(&nerr)) {
		note("validate: %v", nserr(nerr))
		os.Exit(1)
	}
	note("configuration valid")
	// And a bad one, to see the NSError come back.
	bad := alloc("VZVirtualMachineConfiguration").Send(sel("init"))
	nerr = 0
	ok := objc.Send[bool](bad, sel("validateWithError:"), unsafe.Pointer(&nerr))
	note("an empty configuration validates? %v: %v", ok, nserr(nerr))
	pool_pop(p)

	// A delegate class: guestDidStopVirtualMachine: / virtualMachine:didStopWithError:.
	delegateClass, err := objc.RegisterClass("GVXSpikeDelegate", objc.GetClass("NSObject"),
		[]*objc.Protocol{objc.GetProtocol("VZVirtualMachineDelegate")}, nil,
		[]objc.MethodDef{
			{Cmd: sel("guestDidStopVirtualMachine:"), Fn: func(self objc.ID, _ objc.SEL, m objc.ID) {
				v.stopped <- fmt.Sprintf("delegate guestDidStopVirtualMachine: (thread %#x)", pthread_self())
			}},
			{Cmd: sel("virtualMachine:didStopWithError:"), Fn: func(self objc.ID, _ objc.SEL, m objc.ID, e objc.ID) {
				v.stopped <- fmt.Sprintf("delegate didStopWithError: %v", nserr(e))
			}},
		})
	if err != nil {
		note("register delegate: %v", err)
		os.Exit(1)
	}
	delegate := objc.ID(delegateClass).Send(sel("alloc")).Send(sel("init"))

	// The VM is made on, and only touched on, its queue.
	onQueue(q, func() {
		v.obj = alloc("VZVirtualMachine").Send(sel("initWithConfiguration:queue:"), cfg, q)
		v.obj.Send(sel("setDelegate:"), delegate)
	})
	note("VZVirtualMachine made, state %d", v.state())

	// startWithCompletionHandler: — a block called on the VM's queue, a GCD
	// worker thread no Go code made.
	started := make(chan error, 1)
	onQueue(q, func() {
		blk := objc.NewBlock(func(_ objc.Block, e objc.ID) {
			note("start completion handler on thread %#x", pthread_self())
			started <- nserr(e)
		})
		v.obj.Send(sel("startWithCompletionHandler:"), blk)
		blk.Release()
	})
	if err := <-started; err != nil {
		note("start: %v", err)
		os.Exit(1)
	}
	note("started, state %d", v.state())

	// connectToPort:completionHandler: until the agent answers.
	var conn *os.File
	for {
		c, err := v.connect(1024)
		if err == nil {
			conn = c
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	note("vsock connected to port 1024")
	fmt.Fprintf(conn, `{"op":"ping"}`+"\n")
	line, _ := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	note("ping: %q", line)

	run := func(argv ...string) {
		c, err := v.connect(1024)
		if err != nil {
			note("connect: %v", err)
			return
		}
		defer c.Close()
		b, _ := json.Marshal(map[string]any{"op": "exec", "argv": argv})
		c.Write(append(b, '\n'))
		r := bufio.NewReader(c)
		for {
			var hdr [5]byte
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				note("exec: %v", err)
				return
			}
			p := make([]byte, binary.BigEndian.Uint32(hdr[1:]))
			io.ReadFull(r, p)
			switch hdr[0] {
			case 1, 2:
				fmt.Printf("    | %s", p)
			case 3:
				note("exec %v: exit %s", argv, p)
				return
			}
		}
	}
	run("sh", "-c", "mount -t proc proc /mnt/amd64/proc; chroot /mnt/amd64 /bin/sh -c 'uname -m; busybox uname -m'")
	run("sh", "-c", "cat /proc/sys/fs/binfmt_misc/rosetta | head -3; for d in /sys/bus/virtio/devices/*; do echo $d $(cat $d/device); done; sleep 2; ip -4 addr show eth0 | grep inet")

	// A connection held while we read many bytes: throughput, and that a
	// dup'd fd outlives the VZVirtioSocketConnection object.
	{
		c, _ := v.connect(1024)
		b, _ := json.Marshal(map[string]any{"op": "exec", "argv": []string{"sh", "-c", "head -c 67108864 /dev/zero"}})
		c.Write(append(b, '\n'))
		r := bufio.NewReaderSize(c, 1<<20)
		st := time.Now()
		var total int
		for {
			var hdr [5]byte
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				break
			}
			n := int(binary.BigEndian.Uint32(hdr[1:]))
			io.CopyN(io.Discard, r, int64(n))
			if hdr[0] == 3 {
				break
			}
			total += n
		}
		c.Close()
		note("read %d MiB over vsock in %s", total>>20, time.Since(st).Round(time.Millisecond))
	}

	// Many concurrent connects, to see blocks and the trampoline under load.
	{
		var wg sync.WaitGroup
		var okN atomic.Int32
		st := time.Now()
		for range 200 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := v.connect(1024)
				if err != nil {
					return
				}
				fmt.Fprintf(c, `{"op":"ping"}`+"\n")
				if l, _ := bufio.NewReader(c).ReadString('\n'); len(l) > 4 {
					okN.Add(1)
				}
				c.Close()
			}()
		}
		wg.Wait()
		note("200 concurrent pings: %d ok in %s", okN.Load(), time.Since(st).Round(time.Millisecond))
	}
	runtime.GC()

	// A connect to a port nobody listens on: the NSError in the block.
	if _, err := v.connect(4242); err != nil {
		note("connect to port 4242: %v", err)
	}

	if os.Getenv("SPIKE_FORCE") != "" {
		done := make(chan error, 1)
		onQueue(q, func() {
			note("canStop %v", objc.Send[bool](v.obj, sel("canStop")))
			blk := objc.NewBlock(func(_ objc.Block, e objc.ID) { done <- nserr(e) })
			v.obj.Send(sel("stopWithCompletionHandler:"), blk)
			blk.Release()
		})
		note("stopWithCompletionHandler: err=%v", <-done)
		select {
		case s := <-v.stopped:
			note("and the delegate: %s", s)
		case <-time.After(time.Second):
			note("no delegate call after a forced stop (as documented)")
		}
		note("final state %d (0 = stopped)", v.state())
		return
	}
	// Stop 1: requestStopWithError: (a power-button press for the guest).
	onQueue(q, func() {
		var e objc.ID
		can := objc.Send[bool](v.obj, sel("canRequestStop"))
		ok := objc.Send[bool](v.obj, sel("requestStopWithError:"), unsafe.Pointer(&e))
		note("canRequestStop %v, requestStopWithError: %v %v", can, ok, nserr(e))
	})
	select {
	case s := <-v.stopped:
		note("stopped: %s", s)
	case <-time.After(3 * time.Second):
		note("the guest ignored requestStop after 3s (busybox init has no power-button handler); state %d", v.state())
		// Stop 2: the guest powers itself off.
		go run("poweroff", "-f")
		select {
		case s := <-v.stopped:
			note("stopped: %s", s)
		case <-time.After(5 * time.Second):
			note("poweroff -f: no delegate call; state %d", v.state())
			// Stop 3: stopWithCompletionHandler:.
			done := make(chan error, 1)
			onQueue(q, func() {
				blk := objc.NewBlock(func(_ objc.Block, e objc.ID) { done <- nserr(e) })
				v.obj.Send(sel("stopWithCompletionHandler:"), blk)
				blk.Release()
			})
			note("stopWithCompletionHandler: %v", <-done)
		}
	}
	note("final state %d (0 = stopped)", v.state())
}

func (v *vm) state() int64 {
	var s int64
	onQueue(v.q, func() { s = objc.Send[int64](v.obj, sel("state")) })
	return s
}

// connect dials a guest vsock port. The handler gets a
// VZVirtioSocketConnection; its fd is dup'd so the Go side owns a
// descriptor independent of the object's lifetime.
func (v *vm) connect(port uint32) (*os.File, error) {
	type res struct {
		fd  int
		err error
	}
	ch := make(chan res, 1)
	onQueue(v.q, func() {
		devs := v.obj.Send(sel("socketDevices"))
		dev := devs.Send(sel("objectAtIndex:"), uint64(0))
		blk := objc.NewBlock(func(_ objc.Block, c objc.ID, e objc.ID) {
			if e != 0 {
				ch <- res{-1, nserr(e)}
				return
			}
			fd := objc.Send[int32](c, sel("fileDescriptor"))
			nfd, err := syscall.Dup(int(fd))
			if err != nil {
				ch <- res{-1, err}
				return
			}
			// Closing the object closes its fd only, not our dup.
			c.Send(sel("close"))
			ch <- res{nfd, nil}
		})
		dev.Send(sel("connectToPort:completionHandler:"), port, blk)
		blk.Release()
	})
	r := <-ch
	if r.err != nil {
		return nil, r.err
	}
	syscall.CloseOnExec(r.fd)
	if err := syscall.SetNonblock(r.fd, true); err != nil {
		syscall.Close(r.fd)
		return nil, err
	}
	f := os.NewFile(uintptr(r.fd), "vsock")
	if f == nil {
		return nil, errors.New("NewFile")
	}
	return f, nil
}
