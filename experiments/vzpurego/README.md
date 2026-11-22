# vzpurego — Virtualization.framework from Go without cgo, as a spike

The question before the amd64 builder (docs/design/amd64-builder.md): can
the helper drive Virtualization.framework with `CGO_ENABLED=0`, through the
Objective-C runtime (purego and its objc package)? **Yes**: this program
boots the vzrosetta prototype's kernel and initramfs with Rosetta, a
virtiofs share, a virtio-blk disk, NAT and vsock, talks to the prototype's
agent, and stops the VM. The design's section 2 has what worked and what
did not; `internal/vz` is this code made into a package.

```sh
../vzrosetta/build.sh /tmp/gvxd-vzs           # the kernel, initramfs, x86_64 rootfs
GOWORK=off CGO_ENABLED=0 go build -o /tmp/gvxd-vzs/vzpurego .
codesign --sign - --entitlements ../vzrosetta/entitlements.plist --force /tmp/gvxd-vzs/vzpurego
/tmp/gvxd-vzs/vzpurego -work /tmp/gvxd-vzs     # SPIKE_FORCE=1: stop with stopWithCompletionHandler:
```

A module of its own (`go.mod` here), like vzrosetta; build with `GOWORK=off`.
