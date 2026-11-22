#!/bin/sh
# Boot the VM with Rosetta, a NAT device and the buildkit share, start
# buildkitd in it, and leave its socket at $work/buildkitd.sock on the host.
# Needs build.sh and scripts/fetch-buildkit.py to have run. The VM's pid is
# left in $work/run.pid: kill -TERM it to stop the VM.
#
#   usage: buildkit-up.sh [workdir] [more "vzrosetta run" flags…]
set -eu
work=${1:-/tmp/gvxd-vz}
[ $# -gt 0 ] && shift
cd "$work"
build/vzrosetta run -rosetta -net -mem 4096 \
	-sock "$work/agent.sock" \
	-share "buildkit=$work/buildkit:ro" \
	-forward "$work/buildkitd.sock=/run/buildkit/buildkitd.sock" "$@" > run-buildkit.log 2>&1 &
echo $! > run.pid
sleep 3
# The platforms are named: buildkitd's own probe runs an amd64 binary in a
# chroot that has no /proc, where Rosetta cannot start, and so finds none.
build/vzrosetta exec -sock "$work/agent.sock" -- sh -c '
	mkdir -p /run/buildkit /var/lib/buildkit
	export PATH=/mnt/buildkit/bin:$PATH SSL_CERT_FILE=/mnt/buildkit/certs/ca-certificates.crt
	setsid buildkitd --addr unix:///run/buildkit/buildkitd.sock --root /var/lib/buildkit \
		--oci-worker-platform linux/arm64 --oci-worker-platform linux/amd64 \
		> /run/buildkitd.log 2>&1 &
	sleep 1.5
	buildctl debug workers'
