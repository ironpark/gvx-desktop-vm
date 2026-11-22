#!/usr/bin/env python3
"""Download and verify what the vzrosetta experiment boots.

Everything goes under one work directory (default /tmp/gvxd-vz), nothing
into the repository:

  cache/      the downloads, as they came
  keys/       Alpine's package signing keys, from the verified minirootfs
  pkg/NAME/   an unpacked Alpine package
  amd64root/  the x86_64 Alpine minirootfs, unpacked

What is verified, and against what:

  * release tarballs (minirootfs): the .sha256 next to them, over HTTPS;
  * APKINDEX.tar.gz: its RSA signature, with a key that the verified
    aarch64 minirootfs ships in /etc/apk/keys;
  * every .apk: the SHA-1 of its control segment against the C: field of
    the verified index, then the SHA-256 of its data segment against the
    datahash its control segment declares. That is what apk itself checks.

Run it as  python3 -I fetch.py [workdir].
"""

import base64
import hashlib
import os
import shutil
import subprocess
import sys
import tarfile
import io
import urllib.request
import zlib

MIRROR = "https://dl-cdn.alpinelinux.org/alpine"
# The branch the kernel and busybox come from, and the release whose
# minirootfs carries the signing keys.
BRANCH = "v3.24"
RELEASE = "3.24.4"
# The x86_64 userland Rosetta is tried on.
AMD64_BRANCH = "v3.20"
AMD64_RELEASE = "3.20.3"

PACKAGES = ["linux-virt", "busybox-static"]


def log(*a):
    print("fetch:", *a, flush=True)


def download(url, dest):
    if os.path.exists(dest):
        return
    log("GET", url)
    tmp = dest + ".part"
    req = urllib.request.Request(url, headers={"User-Agent": "curl/8"})
    with urllib.request.urlopen(req) as r, open(tmp, "wb") as f:
        shutil.copyfileobj(r, f, 1 << 20)
    os.replace(tmp, dest)


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def fetch_release(cache, branch, arch, name):
    """A release file and its .sha256; returns the verified path."""
    base = f"{MIRROR}/{branch}/releases/{arch}/{name}"
    path = os.path.join(cache, name)
    download(base, path)
    download(base + ".sha256", path + ".sha256")
    want = open(path + ".sha256").read().split()[0]
    got = sha256_file(path)
    if want != got:
        os.remove(path)
        sys.exit(f"{name}: sha256 {got}, the release says {want}")
    log("sha256 ok", name, got)
    return path


def gzip_members(data):
    """Split concatenated gzip streams: [(compressed bytes, plain bytes)]."""
    out = []
    while data:
        d = zlib.decompressobj(wbits=31)
        plain = d.decompress(data)
        if not d.eof:
            sys.exit("truncated gzip stream")
        used = len(data) - len(d.unused_data)
        out.append((data[:used], plain))
        data = d.unused_data
    return out


def tar_members(plain):
    """Members of a tar segment that may lack the end-of-archive blocks."""
    files = {}
    with tarfile.open(fileobj=io.BytesIO(plain + b"\0" * 1024), mode="r:") as t:
        for m in t:
            if m.isfile():
                files[m.name] = t.extractfile(m).read()
    return files


def verify_index(cache, keys, arch):
    """The verified APKINDEX of BRANCH/main as {package: {field: value}}."""
    path = os.path.join(cache, f"APKINDEX-{arch}.tar.gz")
    download(f"{MIRROR}/{BRANCH}/main/{arch}/APKINDEX.tar.gz", path)
    data = open(path, "rb").read()
    members = gzip_members(data)
    sig_files = tar_members(members[0][1])
    (sig_name, sig), = sig_files.items()
    # .SIGN.RSA.<key file> signs with SHA-1, .SIGN.RSA256.<key file> with SHA-256.
    _, _, kind, key_name = sig_name.split(".", 3)
    digest = {"RSA": "-sha1", "RSA256": "-sha256"}[kind]
    key = os.path.join(keys, os.path.basename(key_name))
    if not os.path.exists(key):
        sys.exit(f"APKINDEX is signed by {key_name}, which the minirootfs does not trust")
    signed = data[len(members[0][0]):]
    sig_path = path + ".sig"
    open(sig_path, "wb").write(sig)
    r = subprocess.run(
        ["/usr/bin/openssl", "dgst", digest, "-verify", key, "-signature", sig_path],
        input=signed, capture_output=True)
    if r.returncode != 0 or b"Verified OK" not in r.stdout:
        os.remove(path)
        sys.exit(f"APKINDEX signature: {r.stdout!r} {r.stderr!r}")
    log("signature ok APKINDEX", arch, "by", os.path.basename(key_name))
    index = {}
    text = tar_members(b"".join(p for _, p in members[1:]))["APKINDEX"].decode()
    for block in text.split("\n\n"):
        fields = dict(line.split(":", 1) for line in block.splitlines() if ":" in line)
        if "P" in fields:
            index[fields["P"]] = fields
    return index


def fetch_apk(cache, index, arch, name):
    """Download NAME at the index's version, verify it, return its path."""
    entry = index[name]
    file = f"{name}-{entry['V']}.apk"
    path = os.path.join(cache, file)
    download(f"{MIRROR}/{BRANCH}/main/{arch}/{file}", path)
    data = open(path, "rb").read()
    members = gzip_members(data)
    if len(members) != 3:
        sys.exit(f"{file}: {len(members)} gzip segments, an apk has 3")
    control_gz, control = members[1]
    data_gz = members[2][0]
    want = entry["C"]
    if not want.startswith("Q1"):
        sys.exit(f"{file}: checksum {want} is not SHA-1")
    got = "Q1" + base64.b64encode(hashlib.sha1(control_gz).digest()).decode()
    if got != want:
        os.remove(path)
        sys.exit(f"{file}: control checksum {got}, the index says {want}")
    info = tar_members(control)[".PKGINFO"].decode()
    datahash = [l.split("=", 1)[1].strip() for l in info.splitlines() if l.startswith("datahash")]
    got = hashlib.sha256(data_gz).hexdigest()
    if datahash != [got]:
        os.remove(path)
        sys.exit(f"{file}: data sha256 {got}, the package says {datahash}")
    log("verified", file, "control", want, "data", got[:16] + "…")
    return path


def unpack(archive, dest):
    """Unpack with bsdtar, which refuses to write outside dest."""
    if os.path.exists(dest):
        shutil.rmtree(dest)
    os.makedirs(dest)
    subprocess.run(["/usr/bin/tar", "-xzf", archive, "-C", dest], check=True,
                   stderr=subprocess.DEVNULL)


def main():
    work = sys.argv[1] if len(sys.argv) > 1 else "/tmp/gvxd-vz"
    cache = os.path.join(work, "cache")
    keys = os.path.join(work, "keys")
    os.makedirs(cache, exist_ok=True)

    arm_root = fetch_release(cache, BRANCH, "aarch64",
                             f"alpine-minirootfs-{RELEASE}-aarch64.tar.gz")
    if os.path.exists(keys):
        shutil.rmtree(keys)
    os.makedirs(keys)
    with tarfile.open(arm_root) as t:
        for m in t:
            name = m.name.lstrip("./")
            if name.startswith("etc/apk/keys/") and m.isfile():
                with open(os.path.join(keys, os.path.basename(name)), "wb") as f:
                    f.write(t.extractfile(m).read())
    log("keys", sorted(os.listdir(keys)))

    index = verify_index(cache, keys, "aarch64")
    versions = {}
    for name in PACKAGES:
        apk = fetch_apk(cache, index, "aarch64", name)
        unpack(apk, os.path.join(work, "pkg", name))
        versions[name] = index[name]["V"]

    amd_root = fetch_release(cache, AMD64_BRANCH, "x86_64",
                             f"alpine-minirootfs-{AMD64_RELEASE}-x86_64.tar.gz")
    unpack(amd_root, os.path.join(work, "amd64root"))

    with open(os.path.join(work, "versions.txt"), "w") as f:
        for name, v in versions.items():
            f.write(f"{name} {v}\n")
        f.write(f"alpine-minirootfs-x86_64 {AMD64_RELEASE}\n")
    log("done:", versions)


if __name__ == "__main__":
    main()
