#!/usr/bin/env python3
"""Make what VZLinuxBootLoader boots from what fetch.py unpacked.

  build/Image         the kernel, taken out of Alpine's EFI zboot wrapper:
                      VZLinuxBootLoader wants a bare arm64 Image
  build/initramfs.gz  busybox, the agent, the guest's scripts, and the
                      modules MODULES names with what they depend on

Run it as  python3 -I mkinitramfs.py <workdir> <guest dir> <agent binary>.
The archive is written here, a cpio "newc" member at a time, so that its
files are root's and it has a /dev/console without this being run as root.
"""

import glob
import gzip
import os
import stat
import struct
import sys
import zlib

# Not built into linux-virt (CONFIG_…=m), and needed here:
MODULES = [
    "fuse", "virtiofs",                # the shares, Rosetta's among them
    "binfmt_misc",                     # running x86-64 ELF through Rosetta
    "vsock", "vmw_vsock_virtio_transport",  # the host's way in
    "virtio_rng", "virtio_balloon",
    "virtio_net", "af_packet",         # the NAT device, and udhcpc on it
    "overlay", "veth",                 # buildkitd's snapshots and runc
]


def log(*a):
    print("mkinitramfs:", *a, flush=True)


def unwrap_kernel(src, dst):
    data = open(src, "rb").read()
    if data[:2] == b"MZ" and data[4:8] == b"zimg":
        offset, size = struct.unpack_from("<II", data, 8)
        kind = data[24:32].rstrip(b"\0").decode()
        if kind != "gzip":
            sys.exit(f"kernel: zboot payload is {kind}, only gzip is handled")
        data = zlib.decompressobj(wbits=31).decompress(data[offset:offset + size])
        log(f"kernel: EFI zboot, gzip payload at {offset:#x}, {size} bytes")
    elif data[:2] == b"\x1f\x8b":
        data = zlib.decompressobj(wbits=31).decompress(data)
        log("kernel: gzip")
    if data[56:60] != b"ARM\x64":
        sys.exit("kernel: what came out is not an arm64 Image")
    open(dst, "wb").write(data)
    log(f"kernel: {dst}, {len(data)} bytes")


def module_order(moddir):
    """MODULES and their dependencies, each after what it needs."""
    deps, by_name = {}, {}
    for line in open(os.path.join(moddir, "modules.dep")):
        path, _, needs = line.partition(":")
        deps[path] = needs.split()
        name = os.path.basename(path).split(".ko")[0].replace("-", "_")
        by_name[name] = path
    order = []

    def visit(path):
        if path in order:
            return
        for d in deps[path]:
            visit(d)
        order.append(path)

    for name in MODULES:
        if name not in by_name:
            sys.exit(f"module {name} is not in this kernel package")
        visit(by_name[name])
    return order


class Cpio:
    def __init__(self):
        self.out = bytearray()
        self.ino = 0

    def add(self, name, mode, data=b"", rdev=(0, 0)):
        self.ino += 1
        name_b = name.encode() + b"\0"
        fields = [self.ino, mode, 0, 0, 1, 0, len(data), 0, 0, rdev[0], rdev[1], len(name_b), 0]
        self.out += b"070701" + b"".join(b"%08X" % f for f in fields)
        self.out += name_b
        self.out += b"\0" * (-len(self.out) % 4)
        self.out += data
        self.out += b"\0" * (-len(self.out) % 4)

    def dir(self, name):
        self.add(name, stat.S_IFDIR | 0o755)

    def file(self, name, data, mode=0o644):
        self.add(name, stat.S_IFREG | mode, data)

    def symlink(self, name, target):
        self.add(name, stat.S_IFLNK | 0o777, target.encode())

    def finish(self):
        self.add("TRAILER!!!", 0)
        return bytes(self.out)


def main():
    work, guest, agent = sys.argv[1:4]
    build = os.path.join(work, "build")
    os.makedirs(build, exist_ok=True)
    kpkg = os.path.join(work, "pkg", "linux-virt")
    unwrap_kernel(os.path.join(kpkg, "boot", "vmlinuz-virt"), os.path.join(build, "Image"))

    (moddir,) = glob.glob(os.path.join(kpkg, "lib", "modules", "*"))
    release = os.path.basename(moddir)
    order = module_order(moddir)

    c = Cpio()
    for d in ["bin", "dev", "etc", "lib", "lib/modules", "newroot", "proc"]:
        c.dir(d)
    c.add("dev/console", stat.S_IFCHR | 0o600, rdev=(5, 1))
    busybox = os.path.join(work, "pkg", "busybox-static", "bin", "busybox.static")
    c.file("bin/busybox", open(busybox, "rb").read(), 0o755)
    c.file("bin/vzagent", open(agent, "rb").read(), 0o755)
    c.file("init", open(os.path.join(guest, "init"), "rb").read(), 0o755)
    c.file("etc/rc", open(os.path.join(guest, "rc"), "rb").read(), 0o755)
    c.file("etc/udhcpc.sh", open(os.path.join(guest, "udhcpc.sh"), "rb").read(), 0o755)
    c.file("etc/inittab", open(os.path.join(guest, "inittab"), "rb").read())
    c.file("etc/passwd", b"root:x:0:0:root:/root:/bin/sh\n")
    c.file("etc/group", b"root:x:0:\n")
    c.file("etc/kernel-release", release.encode() + b"\n")
    names, total = [], 0
    for path in order:
        raw = open(os.path.join(moddir, path), "rb").read()
        if path.endswith(".gz"):
            raw = gzip.decompress(raw)
        name = os.path.basename(path).split(".ko")[0] + ".ko"
        c.file("lib/modules/" + name, raw)
        names.append(name)
        total += len(raw)
    c.file("lib/modules/load.list", ("\n".join(names) + "\n").encode())
    log(f"modules of {release} ({total} bytes), in load order:", " ".join(names))

    archive = c.finish()
    dst = os.path.join(build, "initramfs.gz")
    with gzip.open(dst, "wb", compresslevel=6) as f:
        f.write(archive)
    log(f"initramfs: {dst}, {os.path.getsize(dst)} bytes ({len(archive)} unpacked)")


if __name__ == "__main__":
    main()
