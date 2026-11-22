#!/bin/sh
# Build the experiment into a work directory outside the repository
# (default /tmp/gvxd-vz): the host program, signed ad hoc with the
# virtualization entitlement, the guest agent, and the kernel and
# initramfs from what scripts/fetch.py downloads.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
work=${1:-/tmp/gvxd-vz}
mkdir -p "$work/build"

[ -d "$work/pkg/linux-virt" ] || /usr/bin/python3 -I "$here/scripts/fetch.py" "$work"

cd "$here"
# Its own module (go.mod here), outside the app's and any go.work.
export GOWORK=off
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$work/build/vzagent" ./agent
go build -o "$work/build/vzrosetta" .
# Without the entitlement Virtualization.framework refuses to make a VM.
codesign --sign - --entitlements "$here/entitlements.plist" --force "$work/build/vzrosetta"

/usr/bin/python3 -I "$here/scripts/mkinitramfs.py" "$work" "$here/guest" "$work/build/vzagent"
ls -l "$work/build"
