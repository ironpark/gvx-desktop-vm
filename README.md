# gvx-desktop-vm

The Linux VM that [GVX Desktop](https://github.com/ironpark/gvx-desktop) starts
for what its gvx sandboxes cannot do: they run arm64 only, so builds for
`linux/amd64` go to a small VM made with Apple's Virtualization.framework, where
Rosetta for Linux runs x86-64 code.

| Package | What it is |
|---|---|
| `vz` | Virtualization.framework from pure Go (purego, no cgo), macOS on Apple silicon only |
| `vmhelper`, `cmd/gvx-vmhelper` | The helper process that owns a VM; GVX Desktop runs it as `gvx-desktop __vm-helper` |
| `vmproto`, `vmclient` | The app ↔ helper protocol over a 0600 Unix socket, and its client |
| `guestproto`, `guest/agent` | The helper ↔ guest agent protocol over vsock, and the agent |
| `guest/rootfs`, `guest/mkassets` | The guest's init, and the reproducible build of the VM assets |
| `vmassets` | The pinned assets: download, checksum, unpack, verify |
| `experiments/` | The prototypes this came from, each its own module |

## VM assets

The kernel, initramfs, agent and buildkit binaries are never committed. They
are built with

```sh
go run ./guest/mkassets -out /tmp/gvxd-assets
```

from pinned Alpine packages and buildkit images (`guest/mkassets/pins.go`).
Two builds give the same archive. Pushing a tag `vm-assets-v<N>` builds them
in CI and attaches the archive, its corresponding-source archive and their
checksums to that release; `vmassets` pins the archive's SHA-256.
`GVX_VM_ASSETS=<dir>` points at a local build instead.

The assets include GPL and LGPL software; `SOURCES`, `LICENSES/` and the
sources archive on each release carry what their licenses ask for.

## Development

GVX Desktop uses this module through a `go.work` that lists both checkouts.
Programs that use `vz` need the `com.apple.security.virtualization`
entitlement; sign test binaries ad hoc with `entitlements.plist`:

```sh
codesign --sign - --entitlements entitlements.plist --force <binary>
```

Tests that boot a real VM are opt-in: `GVX_VZ_TEST=1` with `GVX_VM_ASSETS`.

Apache-2.0, except the third-party software in the VM assets.
