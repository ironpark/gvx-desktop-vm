//go:build darwin

package vmhelper

import (
	"syscall"
)

func hostMacOSVersion() string {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}
