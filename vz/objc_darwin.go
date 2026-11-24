//go:build darwin && arm64

package vz

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var (
	rtOnce sync.Once
	rtErr  error

	dispatchQueueCreate func(label *byte, attr uintptr) uintptr
	dispatchSyncF       func(q uintptr, ctx uintptr, fn uintptr)
	dispatchRelease     func(obj uintptr)
	poolPush            func() uintptr
	poolPop             func(uintptr)

	secTaskCreateFromSelf          func(allocator uintptr) uintptr
	secTaskCopyValueForEntitlement func(task uintptr, entitlement uintptr, errOut unsafe.Pointer) uintptr
	cfBooleanGetTypeID             func() uintptr
	cfBooleanGetValue              func(b uintptr) bool
	cfGetTypeID                    func(cf uintptr) uintptr
	cfRelease                      func(cf uintptr)
	cfStringCreateWithCString      func(alloc uintptr, cStr *byte, encoding uint32) uintptr

	trampoline uintptr
	handles    sync.Map
	nextHandle atomic.Uintptr

	delegateClass objc.Class
	delegates     sync.Map // objc.ID -> *VM
)

const kCFStringEncodingUTF8 = 0x08000100

func initRuntime() error {
	rtOnce.Do(func() {
		if _, err := purego.Dlopen("/System/Library/Frameworks/Foundation.framework/Foundation", purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
			rtErr = fmt.Errorf("load Foundation: %w", err)
			return
		}
		if _, err := purego.Dlopen("/System/Library/Frameworks/Virtualization.framework/Virtualization", purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
			rtErr = fmt.Errorf("load Virtualization: %w", err)
			return
		}
		libSystem, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_GLOBAL|purego.RTLD_NOW)
		if err != nil {
			rtErr = fmt.Errorf("load libSystem: %w", err)
			return
		}
		libobjc, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_GLOBAL|purego.RTLD_NOW)
		if err != nil {
			rtErr = fmt.Errorf("load libobjc: %w", err)
			return
		}
		purego.RegisterLibFunc(&dispatchQueueCreate, libSystem, "dispatch_queue_create")
		purego.RegisterLibFunc(&dispatchSyncF, libSystem, "dispatch_sync_f")
		purego.RegisterLibFunc(&dispatchRelease, libSystem, "dispatch_release")
		purego.RegisterLibFunc(&poolPush, libobjc, "objc_autoreleasePoolPush")
		purego.RegisterLibFunc(&poolPop, libobjc, "objc_autoreleasePoolPop")

		// Security and CoreFoundation are used to inspect our own entitlements.
		if sec, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_GLOBAL|purego.RTLD_NOW); err == nil {
			if cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_GLOBAL|purego.RTLD_NOW); err == nil {
				purego.RegisterLibFunc(&secTaskCreateFromSelf, sec, "SecTaskCreateFromSelf")
				purego.RegisterLibFunc(&secTaskCopyValueForEntitlement, sec, "SecTaskCopyValueForEntitlement")
				purego.RegisterLibFunc(&cfBooleanGetTypeID, cf, "CFBooleanGetTypeID")
				purego.RegisterLibFunc(&cfBooleanGetValue, cf, "CFBooleanGetValue")
				purego.RegisterLibFunc(&cfGetTypeID, cf, "CFGetTypeID")
				purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
				purego.RegisterLibFunc(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
			}
		}

		// One C callback for every queue hop: purego's callback table is fixed
		// (2000 entries) and never freed, so the Go closure rides in ctx.
		trampoline = purego.NewCallback(func(ctx uintptr) {
			f, ok := handles.LoadAndDelete(ctx)
			if ok {
				f.(func())()
			}
		})

		nsObject := objc.GetClass("NSObject")
		if nsObject == 0 {
			rtErr = errors.New("NSObject class not found")
			return
		}
		var protos []*objc.Protocol
		if p := objc.GetProtocol("VZVirtualMachineDelegate"); p != nil {
			protos = []*objc.Protocol{p}
		}
		cls, err := objc.RegisterClass("GVXVMDelegate", nsObject, protos, nil, []objc.MethodDef{
			{
				Cmd: sel("guestDidStopVirtualMachine:"),
				Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
					withPool(func() {
						if v, ok := delegates.Load(self); ok {
							v.(*VM).markStopped(nil)
						}
					})
				},
			},
			{
				Cmd: sel("virtualMachine:didStopWithError:"),
				Fn: func(self objc.ID, _ objc.SEL, _ objc.ID, e objc.ID) {
					withPool(func() {
						if v, ok := delegates.Load(self); ok {
							v.(*VM).markStopped(nserr(e))
						}
					})
				},
			},
		})
		if err != nil {
			rtErr = fmt.Errorf("register GVXVMDelegate: %w", err)
			return
		}
		delegateClass = cls
	})
	return rtErr
}

// withPool runs f inside an Objective-C autorelease pool on a locked OS
// thread. libobjc's autorelease pool stack is pthread-local, so poolPush and
// poolPop must execute on the same OS thread even if f triggers syscalls or
// preemption.
func withPool(f func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := poolPush()
	defer poolPop(p)
	f()
}

// onQueue runs f synchronously on the serial dispatch queue q inside an
// autorelease pool.
func onQueue(q uintptr, f func()) {
	h := nextHandle.Add(1)
	handles.Store(h, func() {
		withPool(f)
	})
	dispatchSyncF(q, h, trampoline)
}

func sel(s string) objc.SEL { return objc.RegisterName(s) }

func getClass(s string) (objc.ID, error) {
	c := objc.GetClass(s)
	if c == 0 {
		return 0, fmt.Errorf("vz: Objective-C class %s not found", s)
	}
	return objc.ID(c), nil
}

func allocClass(s string) (objc.ID, error) {
	c, err := getClass(s)
	if err != nil {
		return 0, err
	}
	return c.Send(sel("alloc")), nil
}

func autorelease(id objc.ID) objc.ID {
	if id == 0 {
		return 0
	}
	return id.Send(sel("autorelease"))
}

func release(id objc.ID) {
	if id != 0 {
		withPool(func() {
			id.Send(sel("release"))
		})
	}
}

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

func utf8(s objc.ID) string {
	if s == 0 {
		return ""
	}
	return gostr(objc.Send[*byte](s, sel("UTF8String")))
}

// nsstr returns an autoreleased NSString. Must be called inside an autorelease pool.
func nsstr(s string) objc.ID {
	b := append([]byte(s), 0)
	id := objc.ID(objc.GetClass("NSString")).Send(sel("alloc")).Send(sel("initWithUTF8String:"), &b[0])
	runtime.KeepAlive(b)
	return autorelease(id)
}

// nsurl returns an autoreleased file NSURL. Must be called inside an autorelease pool.
func nsurl(path string) objc.ID {
	id := objc.ID(objc.GetClass("NSURL")).Send(sel("alloc")).Send(sel("initFileURLWithPath:"), nsstr(path))
	return autorelease(id)
}

// nsarray returns an autoreleased NSArray. Must be called inside an autorelease pool.
func nsarray(items ...objc.ID) objc.ID {
	cls := objc.ID(objc.GetClass("NSArray"))
	if len(items) == 0 {
		return cls.Send(sel("array"))
	}
	id := cls.Send(sel("alloc")).Send(sel("initWithObjects:count:"), unsafe.Pointer(&items[0]), uint64(len(items)))
	runtime.KeepAlive(items)
	return autorelease(id)
}

func nserr(e objc.ID) error {
	if e == 0 {
		return nil
	}
	desc := e.Send(sel("localizedDescription"))
	domain := e.Send(sel("domain"))
	code := objc.Send[int64](e, sel("code"))
	return fmt.Errorf("%s (%s %d)", utf8(desc), utf8(domain), code)
}

// hasVirtualizationEntitlement reports whether the current process carries
// com.apple.security.virtualization=true.
func hasVirtualizationEntitlement() bool {
	if secTaskCreateFromSelf == nil || secTaskCopyValueForEntitlement == nil {
		return false
	}
	task := secTaskCreateFromSelf(0)
	if task == 0 {
		return false
	}
	defer cfRelease(task)

	keyBytes := append([]byte("com.apple.security.virtualization"), 0)
	key := cfStringCreateWithCString(0, &keyBytes[0], kCFStringEncodingUTF8)
	runtime.KeepAlive(keyBytes)
	if key == 0 {
		return false
	}
	defer cfRelease(key)

	var cfErr uintptr
	val := secTaskCopyValueForEntitlement(task, key, unsafe.Pointer(&cfErr))
	if cfErr != 0 {
		cfRelease(cfErr)
	}
	if val == 0 {
		return false
	}
	defer cfRelease(val)
	if cfGetTypeID(val) != cfBooleanGetTypeID() {
		return false
	}
	return cfBooleanGetValue(val)
}
