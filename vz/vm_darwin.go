//go:build darwin && arm64

package vz

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

const (
	closeWaitStartTimeout = 10 * time.Second
	closeStopTimeout      = 5 * time.Second
)

// Supported reports whether Virtualization.framework is supported on this host
// and this process holds the com.apple.security.virtualization entitlement.
func Supported() bool {
	if err := initRuntime(); err != nil {
		return false
	}
	var supported bool
	withPool(func() {
		cls, err := getClass("VZVirtualMachine")
		if err != nil {
			return
		}
		supported = objc.Send[bool](cls, sel("isSupported"))
	})
	return supported && hasVirtualizationEntitlement()
}

// RosettaAvailability says whether Rosetta for Linux can be shared.
func RosettaAvailability() Availability {
	if err := initRuntime(); err != nil {
		return RosettaNotSupported
	}
	var avail int64
	withPool(func() {
		cls, err := getClass("VZLinuxRosettaDirectoryShare")
		if err != nil {
			return
		}
		avail = objc.Send[int64](cls, sel("availability"))
	})
	switch avail {
	case 1:
		return RosettaNotInstalled
	case 2:
		return RosettaInstalled
	default:
		return RosettaNotSupported
	}
}

// InstallRosetta asks macOS to install Rosetta and waits.
func InstallRosetta(ctx context.Context) error {
	if err := initRuntime(); err != nil {
		return err
	}
	cls, err := getClass("VZLinuxRosettaDirectoryShare")
	if err != nil {
		return ErrUnsupported
	}
	ch := make(chan error, 1)
	withPool(func() {
		blk := objc.NewBlock(func(_ objc.Block, e objc.ID) {
			withPool(func() {
				ch <- nserr(e)
			})
		})
		cls.Send(sel("installRosettaWithCompletionHandler:"), blk)
		blk.Release()
	})
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// VM is a Virtualization.framework Linux virtual machine.
type VM struct {
	// qMu guards q, obj, delegate and closed: callers hold RLock only for the
	// brief onQueue dispatch (never while waiting for completion handlers);
	// Close takes Lock before releasing ObjC objects and the dispatch queue.
	qMu      sync.RWMutex
	q        uintptr
	obj      objc.ID
	delegate objc.ID
	closed   bool

	// startMu guards startDone: non-nil once startWithCompletionHandler: has
	// been dispatched, and closed inside its completion block so Stop/Close
	// never inspect canStop or release ObjC objects mid-start.
	startMu   sync.Mutex
	startDone chan struct{}

	errMu    sync.Mutex
	stopErr  error
	done     chan struct{}
	doneOnce sync.Once

	// Pipes kept alive so guest hvc0 stdin does not see immediate EOF.
	consoleInR *os.File
	consoleInW *os.File
}

// New validates cfg and makes the VM on a private serial dispatch queue.
func New(cfg Config) (*VM, error) {
	if err := initRuntime(); err != nil {
		return nil, err
	}
	if cfg.Kernel == "" {
		return nil, errors.New("vz: kernel path is required")
	}
	if cfg.CPUs <= 0 {
		return nil, errors.New("vz: cpu count must be positive")
	}
	if cfg.MemoryBytes == 0 {
		return nil, errors.New("vz: memory size must be positive")
	}

	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("vz: console pipe: %w", err)
	}
	closePipes := func() {
		_ = inR.Close()
		_ = inW.Close()
	}

	cfgObj, err := buildConfiguration(cfg, inR)
	if err != nil {
		closePipes()
		return nil, err
	}
	defer release(cfgObj)

	label := append([]byte("gvx.vz.vm"), 0)
	q := dispatchQueueCreate(&label[0], 0)
	runtime.KeepAlive(label)
	if q == 0 {
		closePipes()
		return nil, errors.New("vz: dispatch_queue_create failed")
	}

	v := &VM{
		q:          q,
		done:       make(chan struct{}),
		consoleInR: inR,
		consoleInW: inW,
	}

	var mkErr error
	onQueue(q, func() {
		del := objc.ID(delegateClass).Send(sel("alloc")).Send(sel("init"))
		if del == 0 {
			mkErr = errors.New("vz: delegate init failed")
			return
		}
		vmCls, err := allocClass("VZVirtualMachine")
		if err != nil {
			release(del)
			mkErr = err
			return
		}
		obj := vmCls.Send(sel("initWithConfiguration:queue:"), cfgObj, q)
		if obj == 0 {
			release(del)
			mkErr = errors.New("vz: VZVirtualMachine init failed")
			return
		}
		delegates.Store(del, v)
		obj.Send(sel("setDelegate:"), del)
		v.delegate = del
		v.obj = obj
	})
	if mkErr != nil {
		dispatchRelease(q)
		closePipes()
		return nil, mkErr
	}
	return v, nil
}

// buildConfiguration builds and validates a VZVirtualMachineConfiguration.
// The returned object is owned (+1) and must be released by the caller.
func buildConfiguration(cfg Config, consoleIn *os.File) (objc.ID, error) {
	var cfgObj objc.ID
	var buildErr error

	withPool(func() {
		bootAlloc, err := allocClass("VZLinuxBootLoader")
		if err != nil {
			buildErr = err
			return
		}
		boot := autorelease(bootAlloc.Send(sel("initWithKernelURL:"), nsurl(cfg.Kernel)))
		if cfg.Initrd != "" {
			boot.Send(sel("setInitialRamdiskURL:"), nsurl(cfg.Initrd))
		}
		if cfg.CommandLine != "" {
			boot.Send(sel("setCommandLine:"), nsstr(cfg.CommandLine))
		}

		cAlloc, err := allocClass("VZVirtualMachineConfiguration")
		if err != nil {
			buildErr = err
			return
		}
		c := cAlloc.Send(sel("init"))
		fail := func(err error) {
			release(c)
			buildErr = err
		}

		c.Send(sel("setBootLoader:"), boot)
		c.Send(sel("setCPUCount:"), uint64(cfg.CPUs))
		c.Send(sel("setMemorySize:"), cfg.MemoryBytes)

		// Serial console (hvc0). Dup fds with closeOnDealloc:YES so NSFileHandle
		// owns its descriptors independently of the caller's Console *os.File.
		readFD, err := syscall.Dup(int(consoleIn.Fd()))
		if err != nil {
			fail(fmt.Errorf("vz: dup console read fd: %w", err))
			return
		}
		var writeFD int
		if cfg.Console != nil {
			writeFD, err = syscall.Dup(int(cfg.Console.Fd()))
		} else {
			writeFD, err = syscall.Open(os.DevNull, syscall.O_WRONLY, 0)
		}
		if err != nil {
			_ = syscall.Close(readFD)
			fail(fmt.Errorf("vz: console write fd: %w", err))
			return
		}
		syscall.CloseOnExec(readFD)
		syscall.CloseOnExec(writeFD)

		fhAlloc, err := allocClass("NSFileHandle")
		if err != nil {
			_ = syscall.Close(readFD)
			_ = syscall.Close(writeFD)
			fail(err)
			return
		}
		rh := autorelease(fhAlloc.Send(sel("initWithFileDescriptor:closeOnDealloc:"), int32(readFD), true))
		whAlloc, _ := allocClass("NSFileHandle")
		wh := autorelease(whAlloc.Send(sel("initWithFileDescriptor:closeOnDealloc:"), int32(writeFD), true))

		attAlloc, err := allocClass("VZFileHandleSerialPortAttachment")
		if err != nil {
			fail(err)
			return
		}
		att := autorelease(attAlloc.Send(sel("initWithFileHandleForReading:fileHandleForWriting:"), rh, wh))
		serAlloc, err := allocClass("VZVirtioConsoleDeviceSerialPortConfiguration")
		if err != nil {
			fail(err)
			return
		}
		serial := autorelease(serAlloc.Send(sel("init")))
		serial.Send(sel("setAttachment:"), att)
		c.Send(sel("setSerialPorts:"), nsarray(serial))

		// Entropy device.
		entAlloc, err := allocClass("VZVirtioEntropyDeviceConfiguration")
		if err != nil {
			fail(err)
			return
		}
		c.Send(sel("setEntropyDevices:"), nsarray(autorelease(entAlloc.Send(sel("init")))))

		// Memory balloon device.
		if cfg.Balloon {
			balAlloc, err := allocClass("VZVirtioTraditionalMemoryBalloonDeviceConfiguration")
			if err != nil {
				fail(err)
				return
			}
			c.Send(sel("setMemoryBalloonDevices:"), nsarray(autorelease(balAlloc.Send(sel("init")))))
		}

		// virtio-vsock device.
		sockAlloc, err := allocClass("VZVirtioSocketDeviceConfiguration")
		if err != nil {
			fail(err)
			return
		}
		c.Send(sel("setSocketDevices:"), nsarray(autorelease(sockAlloc.Send(sel("init")))))

		// NAT networking.
		if cfg.NAT {
			nicAlloc, err := allocClass("VZVirtioNetworkDeviceConfiguration")
			if err != nil {
				fail(err)
				return
			}
			nic := autorelease(nicAlloc.Send(sel("init")))
			natAlloc, err := allocClass("VZNATNetworkDeviceAttachment")
			if err != nil {
				fail(err)
				return
			}
			nic.Send(sel("setAttachment:"), autorelease(natAlloc.Send(sel("init"))))
			macCls, err := getClass("VZMACAddress")
			if err != nil {
				fail(err)
				return
			}
			nic.Send(sel("setMACAddress:"), macCls.Send(sel("randomLocallyAdministeredAddress")))
			c.Send(sel("setNetworkDevices:"), nsarray(nic))
		}

		// virtiofs shares (Rosetta + host directories).
		var fsDevs []objc.ID
		if cfg.Rosetta {
			rosAlloc, err := allocClass("VZLinuxRosettaDirectoryShare")
			if err != nil {
				fail(err)
				return
			}
			var nerr objc.ID
			ros := autorelease(rosAlloc.Send(sel("initWithError:"), unsafe.Pointer(&nerr)))
			if ros == 0 {
				fail(fmt.Errorf("vz: rosetta share: %w", nserr(nerr)))
				return
			}
			tag := cfg.RosettaTag
			if tag == "" {
				tag = "rosetta"
			}
			fsAlloc, err := allocClass("VZVirtioFileSystemDeviceConfiguration")
			if err != nil {
				fail(err)
				return
			}
			rdev := autorelease(fsAlloc.Send(sel("initWithTag:"), nsstr(tag)))
			rdev.Send(sel("setShare:"), ros)
			fsDevs = append(fsDevs, rdev)
		}
		for _, sh := range cfg.Shares {
			sdAlloc, err := allocClass("VZSharedDirectory")
			if err != nil {
				fail(err)
				return
			}
			sd := autorelease(sdAlloc.Send(sel("initWithURL:readOnly:"), nsurl(sh.Dir), sh.ReadOnly))
			singleAlloc, err := allocClass("VZSingleDirectoryShare")
			if err != nil {
				fail(err)
				return
			}
			single := autorelease(singleAlloc.Send(sel("initWithDirectory:"), sd))
			fsAlloc, err := allocClass("VZVirtioFileSystemDeviceConfiguration")
			if err != nil {
				fail(err)
				return
			}
			sdev := autorelease(fsAlloc.Send(sel("initWithTag:"), nsstr(sh.Tag)))
			sdev.Send(sel("setShare:"), single)
			fsDevs = append(fsDevs, sdev)
		}
		if len(fsDevs) > 0 {
			c.Send(sel("setDirectorySharingDevices:"), nsarray(fsDevs...))
		}

		// virtio-blk storage devices.
		if len(cfg.Disks) > 0 {
			blkDevs := make([]objc.ID, 0, len(cfg.Disks))
			for _, d := range cfg.Disks {
				dattAlloc, err := allocClass("VZDiskImageStorageDeviceAttachment")
				if err != nil {
					fail(err)
					return
				}
				var nerr objc.ID
				datt := autorelease(dattAlloc.Send(sel("initWithURL:readOnly:error:"), nsurl(d.Path), d.ReadOnly, unsafe.Pointer(&nerr)))
				if datt == 0 {
					fail(fmt.Errorf("vz: disk %s: %w", d.Path, nserr(nerr)))
					return
				}
				bdevAlloc, err := allocClass("VZVirtioBlockDeviceConfiguration")
				if err != nil {
					fail(err)
					return
				}
				bdev := autorelease(bdevAlloc.Send(sel("initWithAttachment:"), datt))
				blkDevs = append(blkDevs, bdev)
			}
			c.Send(sel("setStorageDevices:"), nsarray(blkDevs...))
		}

		var nerr objc.ID
		if !objc.Send[bool](c, sel("validateWithError:"), unsafe.Pointer(&nerr)) {
			fail(fmt.Errorf("vz: invalid configuration: %w", nserr(nerr)))
			return
		}
		cfgObj = c
	})

	if buildErr != nil {
		return 0, buildErr
	}
	return cfgObj, nil
}

// Start boots the VM and returns once VZ says it started.
func (v *VM) Start(ctx context.Context) error {
	ch := make(chan error, 1)
	v.qMu.RLock()
	if v.closed || v.q == 0 {
		v.qMu.RUnlock()
		return errors.New("vz: VM is closed")
	}
	v.startMu.Lock()
	if v.startDone != nil {
		v.startMu.Unlock()
		v.qMu.RUnlock()
		return errors.New("vz: Start already called")
	}
	startDone := make(chan struct{})
	v.startDone = startDone
	v.startMu.Unlock()

	onQueue(v.q, func() {
		if v.obj == 0 {
			close(startDone)
			ch <- errors.New("vz: VM is closed")
			return
		}
		blk := objc.NewBlock(func(_ objc.Block, e objc.ID) {
			withPool(func() {
				err := nserr(e)
				close(startDone)
				ch <- err
			})
		})
		v.obj.Send(sel("startWithCompletionHandler:"), blk)
		blk.Release()
	})
	v.qMu.RUnlock()

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitForStart waits until any dispatched startWithCompletionHandler: has
// completed, or ctx expires.
func (v *VM) waitForStart(ctx context.Context) error {
	v.startMu.Lock()
	ch := v.startDone
	v.startMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop pulls the plug (stopWithCompletionHandler:). It waits for any in-flight
// Start to finish first and never calls stopWithCompletionHandler: when canStop
// is false (which would raise an Objective-C exception).
func (v *VM) Stop(ctx context.Context) error {
	if err := v.waitForStart(ctx); err != nil {
		return err
	}

	for {
		ch := make(chan error, 1)
		var (
			alreadyStopped bool
			waitDone       bool
			retryStarting  bool
			dispatched     bool
			cannotStopErr  error
		)

		v.qMu.RLock()
		if v.closed || v.q == 0 {
			v.qMu.RUnlock()
			return nil
		}
		onQueue(v.q, func() {
			if v.obj == 0 {
				alreadyStopped = true
				return
			}
			canStop := objc.Send[bool](v.obj, sel("canStop"))
			if !canStop {
				st := State(objc.Send[int64](v.obj, sel("state")))
				switch st {
				case StateStopped, StateError:
					alreadyStopped = true
				case StateStopping:
					waitDone = true
				case StateStarting:
					retryStarting = true
				default:
					cannotStopErr = fmt.Errorf("vz: cannot stop VM in state %s", st)
				}
				return
			}
			blk := objc.NewBlock(func(_ objc.Block, e objc.ID) {
				withPool(func() {
					ch <- nserr(e)
				})
			})
			v.obj.Send(sel("stopWithCompletionHandler:"), blk)
			blk.Release()
			dispatched = true
		})
		v.qMu.RUnlock()

		switch {
		case alreadyStopped:
			v.markStopped(nil)
			return nil
		case cannotStopErr != nil:
			return cannotStopErr
		case waitDone:
			select {
			case <-v.done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		case retryStarting:
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-v.done:
				timer.Stop()
				return nil
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		case dispatched:
			select {
			case err := <-ch:
				if err == nil {
					// A forced stop does not call the delegate, so close Done here.
					v.markStopped(nil)
				}
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// State is the VM's state now.
func (v *VM) State() State {
	v.qMu.RLock()
	if v.closed || v.q == 0 {
		v.qMu.RUnlock()
		if v.Err() != nil {
			return StateError
		}
		return StateStopped
	}
	var s int64
	onQueue(v.q, func() {
		if v.obj != 0 {
			s = objc.Send[int64](v.obj, sel("state"))
		}
	})
	v.qMu.RUnlock()
	return State(s)
}

// Done is closed when the VM has stopped, by the guest, on an error, or via
// Stop; Err then says whether it failed.
func (v *VM) Done() <-chan struct{} { return v.done }

// Err is why the VM stopped: nil for a clean stop.
func (v *VM) Err() error {
	v.errMu.Lock()
	defer v.errMu.Unlock()
	return v.stopErr
}

// Connect opens a vsock connection to port of the guest.
func (v *VM) Connect(ctx context.Context, port uint32) (net.Conn, error) {
	type res struct {
		fd  int
		err error
	}
	ch := make(chan res, 1)

	v.qMu.RLock()
	if v.closed || v.q == 0 {
		v.qMu.RUnlock()
		return nil, errors.New("vz: VM is closed")
	}
	onQueue(v.q, func() {
		if v.obj == 0 {
			ch <- res{-1, errors.New("vz: VM is closed")}
			return
		}
		devs := v.obj.Send(sel("socketDevices"))
		if devs == 0 || objc.Send[uint64](devs, sel("count")) == 0 {
			ch <- res{-1, errors.New("vz: no socket device")}
			return
		}
		dev := devs.Send(sel("objectAtIndex:"), uint64(0))
		blk := objc.NewBlock(func(_ objc.Block, c objc.ID, e objc.ID) {
			withPool(func() {
				if e != 0 {
					ch <- res{-1, nserr(e)}
					return
				}
				fd := objc.Send[int32](c, sel("fileDescriptor"))
				nfd, err := syscall.Dup(int(fd))
				// Closing the VZVirtioSocketConnection closes its own fd, not our dup;
				// do this unconditionally so a Dup failure never leaks it.
				c.Send(sel("close"))
				if err != nil {
					ch <- res{-1, err}
					return
				}
				ch <- res{nfd, nil}
			})
		})
		dev.Send(sel("connectToPort:completionHandler:"), port, blk)
		blk.Release()
	})
	v.qMu.RUnlock()

	var r res
	select {
	case r = <-ch:
	case <-ctx.Done():
		go func() {
			if late := <-ch; late.err == nil && late.fd >= 0 {
				_ = syscall.Close(late.fd)
			}
		}()
		return nil, ctx.Err()
	}

	if r.err != nil {
		return nil, r.err
	}
	syscall.CloseOnExec(r.fd)
	if err := syscall.SetNonblock(r.fd, true); err != nil {
		_ = syscall.Close(r.fd)
		return nil, err
	}
	f := os.NewFile(uintptr(r.fd), fmt.Sprintf("vsock:%d", port))
	if f == nil {
		_ = syscall.Close(r.fd)
		return nil, errors.New("vz: os.NewFile returned nil")
	}
	return &vsockConn{
		f:      f,
		local:  vsockAddr{cid: 2, port: 0},
		remote: vsockAddr{cid: 3, port: port},
	}, nil
}

// Close stops the VM if an in-flight Start left it running, then releases the
// VM's Objective-C objects, dispatch queue, and console pipes.
func (v *VM) Close() error {
	v.qMu.RLock()
	alreadyClosed := v.closed
	v.qMu.RUnlock()
	if alreadyClosed {
		return nil
	}

	// Wait (bounded) for any in-flight startWithCompletionHandler: so we never
	// release v.obj or v.q while VZ still has a start callback pending, and
	// stop the VM if it came up so no XPC process is orphaned.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), closeWaitStartTimeout)
	startSettled := v.waitForStart(waitCtx) == nil
	waitCancel()

	if startSettled {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), closeStopTimeout)
		_ = v.Stop(stopCtx)
		stopCancel()
	}

	v.qMu.Lock()
	if v.closed {
		v.qMu.Unlock()
		return nil
	}
	v.closed = true
	q := v.q
	v.q = 0
	inR := v.consoleInR
	inW := v.consoleInW
	v.consoleInR = nil
	v.consoleInW = nil
	if q != 0 && startSettled {
		onQueue(q, func() {
			if v.obj != 0 {
				v.obj.Send(sel("setDelegate:"), objc.ID(0))
				release(v.obj)
				v.obj = 0
			}
			if v.delegate != 0 {
				delegates.Delete(v.delegate)
				release(v.delegate)
				v.delegate = 0
			}
		})
		dispatchRelease(q)
	}
	v.qMu.Unlock()

	v.markStopped(nil)

	if inW != nil {
		_ = inW.Close()
	}
	if inR != nil {
		_ = inR.Close()
	}
	return nil
}

func (v *VM) markStopped(err error) {
	v.doneOnce.Do(func() {
		v.errMu.Lock()
		v.stopErr = err
		v.errMu.Unlock()
		close(v.done)
	})
}
