// Command gvx-vmhelper is the amd64 builder's VM helper as a program of its
// own, for development and the engine's tests; the app runs the same code
// as its own executable with the argument __vm-helper. Sign it before use:
//
//	codesign --sign - --entitlements entitlements.plist --force gvx-vmhelper
//
// See docs/design/amd64-builder.md. Owner: workstream A.
package main

import (
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop-vm/vmhelper"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = append([]string{vmhelper.CmdServe}, args...)
	}
	os.Exit(vmhelper.Main(args))
}
