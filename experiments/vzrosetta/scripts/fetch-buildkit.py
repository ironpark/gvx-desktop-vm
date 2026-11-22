#!/usr/bin/env python3
"""Get buildkitd out of the moby/buildkit image without a container engine.

The image's linux/arm64 layers are pulled from Docker Hub with curl, over
the registry API; the manifest and every layer are checked against the
sha256 digest they were asked for by. Out of the layers come only

  buildkit/bin/buildkitd, buildkit-runc, buildctl   (static binaries)
  buildkit/certs/ca-certificates.crt                (to pull base images)

under the work directory, which run shares with the guest.

Run it as  python3 -I fetch-buildkit.py [workdir] [tag].
"""

import hashlib
import json
import os
import subprocess
import sys
import tarfile

REGISTRY = "https://registry-1.docker.io"
REPO = "moby/buildkit"
WANT = {
    "usr/bin/buildkitd": "bin/buildkitd",
    "usr/bin/buildkit-runc": "bin/buildkit-runc",
    "usr/bin/buildctl": "bin/buildctl",
    "etc/ssl/certs/ca-certificates.crt": "certs/ca-certificates.crt",
}
ACCEPT = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])


def log(*a):
    print("fetch-buildkit:", *a, flush=True)


def curl(url, *headers, out=None):
    cmd = ["/usr/bin/curl", "-fsSL", "--proto", "=https", "--retry", "3"]
    for h in headers:
        cmd += ["-H", h]
    if out:
        cmd += ["-o", out]
    cmd.append(url)
    return subprocess.run(cmd, check=True, capture_output=not out).stdout


def digest_of(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def main():
    work = sys.argv[1] if len(sys.argv) > 1 else "/tmp/gvxd-vz"
    tag = sys.argv[2] if len(sys.argv) > 2 else "v0.34.2"
    cache = os.path.join(work, "cache", "buildkit-" + tag)
    dest = os.path.join(work, "buildkit")
    os.makedirs(cache, exist_ok=True)

    token = json.loads(curl(
        f"https://auth.docker.io/token?service=registry.docker.io&scope=repository:{REPO}:pull"))["token"]
    auth = "Authorization: Bearer " + token

    index = json.loads(curl(f"{REGISTRY}/v2/{REPO}/manifests/{tag}", auth, "Accept: " + ACCEPT))
    (entry,) = [m for m in index["manifests"]
                if m["platform"]["os"] == "linux" and m["platform"]["architecture"] == "arm64"]
    raw = curl(f"{REGISTRY}/v2/{REPO}/manifests/{entry['digest']}", auth, "Accept: " + ACCEPT)
    if digest_of(raw) != entry["digest"]:
        sys.exit("the manifest is not what its digest names")
    manifest = json.loads(raw)
    log(f"{REPO}:{tag} linux/arm64 is {entry['digest']}, {len(manifest['layers'])} layers")

    found = {}
    for layer in manifest["layers"]:
        d = layer["digest"]
        path = os.path.join(cache, d.replace(":", "-") + ".tar.gz")
        if not os.path.exists(path):
            curl(f"{REGISTRY}/v2/{REPO}/blobs/{d}", auth, out=path + ".part")
            os.replace(path + ".part", path)
        h = hashlib.sha256()
        with open(path, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                h.update(chunk)
        if "sha256:" + h.hexdigest() != d:
            os.remove(path)
            sys.exit(f"layer {d}: the bytes are sha256:{h.hexdigest()}")
        log("layer ok", d[:19] + "…", layer["size"], "bytes")
        # A later layer's file replaces an earlier one's.
        with tarfile.open(path) as t:
            for m in t:
                name = m.name.lstrip("./")
                if name in WANT and m.isfile():
                    found[name] = t.extractfile(m).read()

    for name, rel in WANT.items():
        if name not in found:
            sys.exit(f"{name} is in no layer")
        out = os.path.join(dest, rel)
        os.makedirs(os.path.dirname(out), exist_ok=True)
        with open(out, "wb") as f:
            f.write(found[name])
        os.chmod(out, 0o755 if rel.startswith("bin/") else 0o644)
        log("wrote", out, len(found[name]), "bytes")


if __name__ == "__main__":
    main()
