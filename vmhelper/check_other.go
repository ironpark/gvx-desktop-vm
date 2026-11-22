//go:build !darwin

package vmhelper

func hostMacOSVersion() string {
	return ""
}
