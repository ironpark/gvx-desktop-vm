# vzrosetta — a Virtualization.framework VM with Rosetta, as an experiment

A prototype, not part of the app: is a small Linux VM made with
Virtualization.framework (VZ), with Rosetta for Linux, a workable way to run
amd64 images and cross-architecture builds beside the gvx sandboxes?

**Yes.** On an M1 Ultra, macOS 27.2.1, all four steps work:

| | Result |
|---|---|
| 1. Boot | VZLinuxBootLoader + Alpine `linux-virt` 6.18.64 + a 2.5 MB initramfs. The agent answers **0.31–0.35 s** after `start`. The VM process holds **~200 MiB** RSS at 2 GiB configured. |
| 2. Rosetta | `uname -m` is `x86_64` for a shell and every child. The 300000-iteration shell loop: **0.37 s native, 0.60 s under Rosetta** (QEMU user mode in a gvx sandbox: 4.85 s). |
| 3. Host ↔ VM | virtio-vsock to a 3.3 MB static Go agent: ping, run a command, or join a guest Unix socket. A round trip is 0.8–2.3 ms. |
| 4. buildkit | buildkitd v0.34.2 in the VM, reached from the host through a vsock-forwarded Unix socket by a buildx `remote` builder: `--platform linux/amd64` builds in 2.5–3.8 s, the image is `amd64`, its `RUN uname -m` recorded `x86_64`. |

Nothing here touches `~/.gvx`, `~/.docker` or the app. Everything downloaded or
built goes to a work directory outside the repository, `/tmp/gvxd-vz` by
default (short on purpose: a Unix socket path may be 104 bytes at most).

## What is here

| | |
|---|---|
| `go.mod` | A module of its own, so that the app's `go.mod` does not carry vz: build here with `GOWORK=off`, as `build.sh` does. |
| `main.go`, `vm.go` | `vzrosetta`, the host program (`darwin && arm64 && cgo` only): `run` boots the VM, `ping` and `exec` talk to the agent. Uses `github.com/Code-Hex/vz/v3` v3.8.0. |
| `agent/` | The guest agent (`linux` only), built static for linux/arm64. |
| `proto/` | What the two say to each other over vsock. |
| `guest/` | The guest's `/init`, `/etc/rc`, `/etc/inittab` and the udhcpc script. |
| `scripts/fetch.py` | Downloads and verifies the Alpine packages and the x86_64 minirootfs. |
| `scripts/mkinitramfs.py` | Unwraps the kernel and writes the initramfs. |
| `scripts/fetch-buildkit.py` | Pulls buildkitd out of `moby/buildkit` over the registry API. |
| `scripts/buildkit-up.sh` | Boots the VM for step 4 and starts buildkitd in it. |
| `build.sh` | All of the above but buildkit, and the ad-hoc signature. |
| `entitlements.plist` | `com.apple.security.virtualization`, which VZ refuses to work without. |

## Steps

Needs Xcode's command line tools (vz is cgo and Objective-C), Rosetta for
Linux (`softwareupdate --install-rosetta`), and for step 4 `docker` with the
buildx plugin — the CLI only, no engine.

```sh
cd experiments/vzrosetta
./build.sh                       # ~50 MB of downloads, then the build; into /tmp/gvxd-vz
W=/tmp/gvxd-vz; V=$W/build/vzrosetta
LOOP='i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done'
```

`run` stays in the foreground with the guest's console on stdout, each line
after the time since `start`; `kill -TERM` or ^C stops the VM. Run it in one
terminal (or with `&`) and the rest in another.

```sh
# 1. boot, 3. the vsock channel
$V run                           # add -verbose for the kernel's log
$V ping                          # pong Linux 6.18.64-0-virt aarch64
$V exec -time -- sh -c "$LOOP"   # the native timing

# 2. Rosetta
$V run -rosetta -share amd64=$W/amd64root:ro
$V exec -- mount -t proc proc /mnt/amd64/proc
$V exec -- chroot /mnt/amd64 /bin/sh -c 'uname -m; busybox uname -m; apk --version'
$V exec -time -- chroot /mnt/amd64 /bin/sh -c "$LOOP"

# 4. buildkit
python3 -I scripts/fetch-buildkit.py $W v0.34.2     # ~103 MB of layers
scripts/buildkit-up.sh $W                           # the VM's pid is in $W/run.pid
export BUILDX_CONFIG=$W/buildx DOCKER_CONFIG=$W/dockercfg   # never ~/.docker
mkdir -p $DOCKER_CONFIG/cli-plugins $W/ctx $W/out
ln -sf /opt/homebrew/lib/docker/cli-plugins/docker-buildx $DOCKER_CONFIG/cli-plugins/
printf 'FROM alpine:3.20\nRUN uname -m > /arch\n' > $W/ctx/Dockerfile
docker buildx create --name gvxvz --driver remote unix://$W/buildkitd.sock
docker buildx build --builder gvxvz --platform linux/amd64 \
    -o type=docker,dest=$W/out/amd64.tar $W/ctx
kill -TERM $(cat $W/run.pid)

rm -rf /tmp/gvxd-vz              # everything downloaded and built
```

This whole sequence was run from an empty work directory after the code was
last changed. The work directory was then removed; `./build.sh` and
`fetch-buildkit.py` bring everything back.

### What is downloaded, and how it is checked

- `alpine-minirootfs-3.24.4-aarch64.tar.gz` and
  `alpine-minirootfs-3.20.3-x86_64.tar.gz`: against the `.sha256` beside
  them. The first is only used for the package signing keys in its
  `/etc/apk/keys`; the second is the x86_64 userland of step 2.
- `v3.24/main/aarch64/APKINDEX.tar.gz`: its RSA signature, with one of those
  keys.
- `linux-virt` (6.18.64-r0 when this was run) and `busybox-static`
  (1.37.0-r31): the SHA-1 of the control segment against the index's `C:`
  field, then the SHA-256 of the data segment against the `datahash` the
  control segment declares — what `apk` itself checks. The version is
  whatever the signed index names that day; nothing is pinned.
- `moby/buildkit:v0.34.2`, linux/arm64: the manifest and each layer against
  the sha256 digest it is asked for by. The tag itself is trusted to Docker
  Hub over TLS.

## Measurements

Apple M1 Ultra, 64 GB, macOS 27.2.1; VM with 4 vCPUs. Times after `+` are
since `VZVirtualMachine.start` was called.

### 1. Boot

Five boots in a row, 2 GiB, `quiet`, no shares:

| | |
|---|---|
| `start` returns | +0.125–0.130 s |
| `/init` runs (guest uptime 0.07 s) | +0.273–0.278 s |
| `rc` done: 15 modules loaded, filesystems mounted | +0.310–0.326 s |
| the agent accepts on vsock | **+0.316–0.335 s** |

With `-rosetta` and a share it is +0.33–0.34 s, with `-net -mem 4096`
+0.36–0.37 s (DHCP goes on in the background, about a second more).

Memory. The guest runs in an XPC service of Virtualization.framework
(`com.apple.Virtualization.VirtualMachine`), not in the program that made it;
`vzrosetta` itself stays at 18–20 MiB.

| Configured | VM process RSS when the agent answers | `phys_footprint` |
|---|---|---|
| 512 MiB | 163 MiB | 126 MB |
| 1 GiB | 177 MiB | 140 MB |
| 2 GiB | 196–200 MiB | 159–164 MB |
| 4 GiB | 302–320 MiB | 266 MB |

It grows as the guest touches memory and does not come back: 266 MiB after
the Rosetta tests (2 GiB VM), 626–639 MiB (footprint 455–466 MB) after the
buildkit builds (4 GiB VM, buildkitd's state on tmpfs). The guest itself
reported 49 MiB used of 1971 after boot. A balloon device is configured but
nothing drives it.

Sizes: `Image` 36.4 MB (10.4 MB gzipped in the package), `initramfs.gz`
2.5 MB (busybox 1.1 MB, agent 3.3 MB, modules 1.6 MB before compression),
`vzrosetta` 7.4 MB.

### 2. Rosetta

`VZLinuxRosettaDirectoryShare.availability` is
`LinuxRosettaAvailabilityInstalled` (`vz.LinuxRosettaDirectoryShareAvailability()`;
`run` prints it before anything else and refuses `-rosetta` otherwise).
`/proc/sys/fs/binfmt_misc/rosetta` after registration:

```
enabled
interpreter /mnt/rosetta/rosetta
flags: OCF
magic 7f454c4602010100000000000000000002003e00
mask fffffffffffefe00fffffffffffffffffeffffff
```

In a chroot of the x86_64 Alpine 3.20.3 minirootfs, shared over virtiofs:

```
$ chroot /mnt/amd64 /bin/sh -c 'uname -m; busybox uname -m; apk --version'
x86_64
x86_64
apk-tools 2.14.4, compiled for x86_64.
```

`i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done`, wall time as the agent
measures the command:

| | Seconds |
|---|---|
| arm64 busybox in a gvx sandbox (measured before this experiment) | 0.51 |
| qemu-x86_64 in a gvx sandbox (measured before this experiment) | 4.85 |
| arm64 busybox in this VM (8 runs) | **0.36–0.38** |
| x86_64 busybox under Rosetta in this VM (4 runs) | **0.59–0.62** |
| the same in a buildkit `RUN` (runc, seccomp): arm64 / amd64 | 0.52 / 0.59–0.60 |

The two busyboxes differ (static 1.37 from Alpine 3.24 against dynamic 1.36
from 3.20; the `RUN` rows use Alpine 3.20's for both), so read 0.37 against
0.60 as "about 1.6×", no finer.

Starting a process is where Rosetta costs. 200 × `busybox true` from a shell
loop:

| | Seconds | Per exec |
|---|---|---|
| arm64, static, from tmpfs | 0.039 | 0.2 ms |
| arm64, static, from a virtiofs share | 0.45 | 2.2 ms |
| x86_64 under Rosetta, root on virtiofs | 2.6–2.7 | 13 ms |
| x86_64 under Rosetta, root on tmpfs, with rosettad | 1.9–2.0 | 10 ms |

Roughly 10 ms of every x86_64 exec is Rosetta starting, and 2–3 ms more is
virtiofs. `-rosetta-cache` (VZLinuxRosettaAbstractSocketCachingOptions and
`rosettad daemon` in the guest, macOS 14) did write `.aotcache` files but
bought almost nothing here — 2.4–2.5 s for the exec test on virtiofs — and the
loop got slower with it, 0.74 s. It is off by default.

### 3. Host ↔ VM

`run` listens on `$W/agent.sock` and joins each connection to a new vsock
connection to port 1024 of the guest (`VZVirtioSocketDevice.connectToPort`).
`ping` through it takes 0.8–2.3 ms, connection setup included. `-forward
host.sock=guest.sock` does the same and has the agent connect to a Unix
socket in the guest first: that is how buildkitd's socket reaches the host.

### 4. buildkit

`moby/buildkit:v0.34.2`'s `buildkitd`, `buildkit-runc` and `buildctl` are
static: they run straight from a read-only virtiofs share, with the image's
CA bundle for pulling. The worker is `oci` with the overlayfs snapshotter,
its state on the root tmpfs, host networking, behind a NAT device (`-net`).

```
$ docker buildx ls
gvxvz        remote
 \_ gvxvz0    \_ unix:///tmp/gvxd-vz/buildkitd.sock   running   v0.34.2    linux/amd64, linux/arm64
```

A three-line Dockerfile (`FROM alpine:3.20`, a `RUN uname -m > /arch …`, a
`RUN apk add --no-cache file …`) built for `linux/amd64` from nothing in
3.8 s; the two-line one of the steps above in 2.5 s. In the exported tar the
config's `architecture` is `amd64`, and `/arch` holds `x86_64`,
`apk-tools 2.14.4, compiled for x86_64.` and `file`'s word that
`/bin/busybox` is an x86-64 ELF.

## What it took

- **Entitlement.** `com.apple.security.virtualization`, ad-hoc signed
  (`codesign --sign - --entitlements entitlements.plist`). Nothing else: NAT
  networking needs none.
- **VZ APIs** (through vz v3.8.0): VZLinuxBootLoader,
  VZVirtioConsoleDeviceSerialPortConfiguration with a file handle attachment
  (`hvc0`), VZVirtioFileSystemDeviceConfiguration with VZSingleDirectoryShare
  and VZLinuxRosettaDirectoryShare, VZVirtioSocketDeviceConfiguration,
  VZNATNetworkDeviceAttachment, entropy and balloon devices. No run loop on
  the main thread is needed without a display.
- **Kernel.** Alpine's `linux-virt` as shipped. Built in: virtio-pci, the
  virtio console, devtmpfs, cgroup v2, seccomp, user namespaces. Loaded from
  the initramfs (`CONFIG_…=m`), in dependency order with `insmod`: `fuse`,
  `virtiofs`, `binfmt_misc`, `vsock`, `vmw_vsock_virtio_transport` (+
  `_common`), `virtio_rng` (+ `rng-core`), `virtio_balloon`, `virtio_net`
  (+ `failover`, `net_failover`), `af_packet`, `overlay`, `veth`. 4K pages,
  48-bit VA.
- **binfmt_misc.** Apple's documented registration, as the kernel takes it:
  `:rosetta:M::<magic>:<mask>:/mnt/rosetta/rosetta:CF` — `C` for
  `--credentials yes`, `F` for `--fix-binary yes` (the kernel shows `OCF`).
  `F` is what makes it work in a chroot and in runc's mount namespaces.

## Problems, and what solved them

1. **The kernel is not an Image.** Alpine's arm64 `vmlinuz-virt` is an EFI
   zboot PE (`MZ`, `zimg`, a gzip payload). VZLinuxBootLoader wants the bare
   Image: `mkinitramfs.py` reads the payload's offset and size from the header
   and gunzips it.
2. **Modules are gzipped and signed.** busybox `insmod` wants them plain:
   they are gunzipped into the initramfs (the signature rides along) with a
   load order worked out from `modules.dep`.
3. **The initramfs root cannot be pivoted out of**, which runc does for every
   container. `/init` copies itself to a tmpfs and `switch_root`s before
   anything else. (Done from the start; running runc on the bare rootfs was
   not tried.)
4. **`rosetta error: Unable to open /proc/self/exe: 2`** in a chroot without
   `/proc`. Rosetta needs `/proc` wherever it runs; mount it.
5. **buildkitd saw only `linux/arm64`.** Its probe
   (`util/archutil/check_unix.go`) runs an amd64 binary chrooted into an
   empty temporary directory — problem 4 again. Naming the platforms
   (`--oci-worker-platform linux/arm64 --oci-worker-platform linux/amd64`)
   skips the probe.
6. **Socket paths.** A session scratch directory is longer than a Unix socket
   path may be; hence `/tmp/gvxd-vz`.
7. **`docker buildx` with a temporary `DOCKER_CONFIG`** no longer finds
   Homebrew's plugin: link it into `$DOCKER_CONFIG/cli-plugins`. The
   `default` builder then shows an error for want of an engine, which the
   remote builder does not mind.
8. **No half close on VZVirtioSocketConnection.** The forwarder closes the
   whole connection when the client stops writing. gRPC does not care; a
   protocol that half-closes and then reads would.

## Not done

- No disk: buildkitd's cache and everything else are in RAM and gone with the
  VM. A product needs a virtio-blk image (`ext4` and `virtio_blk` are modules
  of this kernel; nothing here makes the filesystem).
- Memory never returns to the host (see the balloon above).
- No container runtime beyond buildkit's runc: running an amd64 image as a
  long-lived container (ports, logs, exec, volumes) was not built.
- Shutdown is `VZVirtualMachine.stop`, i.e. pulling the plug.
- Shared files show up root-owned in the guest whoever owns them on the
  host, and the host volume is case-insensitive: fine for binaries, not a
  place for container root filesystems.
- x86_64 Macs, and macOS before 13: no Rosetta share. Not handled, only
  reported.
