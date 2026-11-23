package main

import "fmt"

const (
	alpineMirror    = "https://dl-cdn.alpinelinux.org/alpine"
	alpineBranch    = "v3.24"
	alpineAportsRaw = "https://gitlab.alpinelinux.org/alpine/aports/-/raw"
	alpineAportsUI  = "https://gitlab.alpinelinux.org/alpine/aports/-/tree"
	alpineAportsAPI = "https://gitlab.alpinelinux.org/api/v4/projects/alpine%2Faports/repository/archive.tar.gz"
)

// apkPin pins one Alpine aarch64 package by exact version, immutable aports
// commit SHA, and SHA-256. mkassets downloads the .apk directly by URL and
// never reads APKINDEX.
type apkPin struct {
	Name         string
	Version      string
	Origin       string
	AportsCommit string
	License      string
	UpstreamURL  string
	SHA256       string
}

func (p apkPin) Filename() string {
	return fmt.Sprintf("%s-%s.apk", p.Name, p.Version)
}

func (p apkPin) URL() string {
	return fmt.Sprintf("%s/%s/main/aarch64/%s", alpineMirror, alpineBranch, p.Filename())
}

func (p apkPin) AportsURL() string {
	return fmt.Sprintf("%s/%s/main/%s", alpineAportsUI, p.AportsCommit, p.Origin)
}

// pinnedAPKs are all Alpine packages that go into the kernel and initramfs.
// To bump when Alpine drops an old package from its branch mirror, update
// Version, AportsCommit, SHA256 (and the corresponding aportsRecipes entry)
// and bump vmassets.Version.
var pinnedAPKs = []apkPin{
	{
		Name:         "linux-virt",
		Version:      "6.18.64-r0",
		Origin:       "linux-lts",
		AportsCommit: "d65a2d332a8ba2153c1c732411ddd44f8dd2beb8",
		License:      "GPL-2.0-only",
		UpstreamURL:  "https://www.kernel.org",
		SHA256:       "cf171f51ab44d637a7758dd5a2f6a4ecd7ddbea18d61b9e37a6d39357d2fb1d5",
	},
	{
		Name:         "busybox-static",
		Version:      "1.37.0-r31",
		Origin:       "busybox",
		AportsCommit: "c3ef5d10e6ef6528852c51f0564963e2f8c1be19",
		License:      "GPL-2.0-only",
		UpstreamURL:  "https://busybox.net/",
		SHA256:       "965777e06b94bf11981d5f4ecdfcd577879f4b0dda544294d1dd3f72b217bc75",
	},
	{
		Name:         "e2fsprogs",
		Version:      "1.47.4-r0",
		Origin:       "e2fsprogs",
		AportsCommit: "8683a6972cb245fbbdb535dad6435a9930bb9374",
		License:      "GPL-2.0-or-later AND LGPL-2.0-or-later AND BSD-3-Clause AND MIT",
		UpstreamURL:  "https://e2fsprogs.sourceforge.net/",
		SHA256:       "52fd401e79ce6b0ff7648733ba4de3135083cd0132543534e72106e7ea767a00",
	},
	{
		Name:         "e2fsprogs-libs",
		Version:      "1.47.4-r0",
		Origin:       "e2fsprogs",
		AportsCommit: "8683a6972cb245fbbdb535dad6435a9930bb9374",
		License:      "GPL-2.0-or-later AND LGPL-2.0-or-later AND BSD-3-Clause AND MIT",
		UpstreamURL:  "https://e2fsprogs.sourceforge.net/",
		SHA256:       "52124971ae397d599aadd79263fcd68fc9e271922ccf8185b124f35fe24c60c8",
	},
	{
		Name:         "libcom_err",
		Version:      "1.47.4-r0",
		Origin:       "e2fsprogs",
		AportsCommit: "8683a6972cb245fbbdb535dad6435a9930bb9374",
		License:      "GPL-2.0-or-later AND LGPL-2.0-or-later AND BSD-3-Clause AND MIT",
		UpstreamURL:  "https://e2fsprogs.sourceforge.net/",
		SHA256:       "a75b5955ff57046e33fc2842cfe466dc77d8e68305d5c86c6646b38a069490a2",
	},
	{
		Name:         "blkid",
		Version:      "2.42.3-r1",
		Origin:       "util-linux",
		AportsCommit: "d98c55af59055e6ca60fbe36e171546918709965",
		License:      "GPL-2.0-or-later",
		UpstreamURL:  "https://git.kernel.org/cgit/utils/util-linux/util-linux.git",
		SHA256:       "680c17cd5c482c15a699abda8a28498dcde9a0b7f2b1ec3e1c51de3f99a03ab7",
	},
	{
		Name:         "libblkid",
		Version:      "2.42.3-r1",
		Origin:       "util-linux",
		AportsCommit: "d98c55af59055e6ca60fbe36e171546918709965",
		License:      "LGPL-2.1-or-later",
		UpstreamURL:  "https://git.kernel.org/cgit/utils/util-linux/util-linux.git",
		SHA256:       "9d0f24976c575c53c12526b6e078edf34c980c153bc8e5f1c1053cbece5b8cfa",
	},
	{
		Name:         "libuuid",
		Version:      "2.42.3-r1",
		Origin:       "util-linux",
		AportsCommit: "d98c55af59055e6ca60fbe36e171546918709965",
		License:      "BSD-3-Clause",
		UpstreamURL:  "https://git.kernel.org/cgit/utils/util-linux/util-linux.git",
		SHA256:       "9ce20c7ffe2ccaa7c321893c10564abbca13c3f2edb82f60a35f1f68e004f86c",
	},
	{
		Name:         "libeconf",
		Version:      "0.8.3-r0",
		Origin:       "libeconf",
		AportsCommit: "62db33049768522f381410614d59c7d732f72c3a",
		License:      "MIT",
		UpstreamURL:  "https://github.com/openSUSE/libeconf",
		SHA256:       "cf0899d49ded0a6891e3cdf1b37887f2ecd77293afe1d8f3cb8937763c83f183",
	},
	{
		Name:         "musl",
		Version:      "1.2.6-r3",
		Origin:       "musl",
		AportsCommit: "8f79d39391b46d6dbf1800fb68b8212a21d5ee2e",
		License:      "MIT",
		UpstreamURL:  "https://musl.libc.org/",
		SHA256:       "2e17864102278d9049c3a56f1bceed9e8cf287065617e579325123443daed8a5",
	},
}

// upstreamSource pins one upstream source tarball listed by a pinned Alpine
// APKBUILD, verified by the exact SHA-512 from that APKBUILD.
type upstreamSource struct {
	Filename string // filename as named in APKBUILD's sha512sums
	URL      string
	// Mirrors are tried in order when URL fails; the SHA-512 pins the file
	// whichever serves it.
	Mirrors []string
	SHA512  string
}

// alpineDistfiles is Alpine's own cache of the upstream sources its
// builders fetched. gitlab.alpinelinux.org answers CI runners with HTTP
// 418, so a source hosted there names its copy here as a mirror.
const alpineDistfiles = "https://distfiles.alpinelinux.org/distfiles/edge/"

// aportsRecipe pins an Alpine aports recipe at an immutable commit SHA, its
// exact APKBUILD SHA-256, and its upstream source tarballs for corresponding
// source archival.
type aportsRecipe struct {
	Origin          string
	Version         string
	License         string
	Commit          string
	APKBUILDSHA256  string
	UpstreamSources []upstreamSource
}

func (r aportsRecipe) TreeURL() string {
	return fmt.Sprintf("%s/%s/main/%s", alpineAportsUI, r.Commit, r.Origin)
}

func (r aportsRecipe) RawFileURL(name string) string {
	return fmt.Sprintf("%s/%s/main/%s/%s", alpineAportsRaw, r.Commit, r.Origin, name)
}

func (r aportsRecipe) ArchiveURL() string {
	return fmt.Sprintf("%s?sha=%s&path=main/%s", alpineAportsAPI, r.Commit, r.Origin)
}

// MirrorArchiveURL is the whole aports tree at the commit from GitHub's
// mirror, for when gitlab.alpinelinux.org turns a CI runner away; the
// recipe is read from it by path and checked against APKBUILDSHA256.
func (r aportsRecipe) MirrorArchiveURL() string {
	return fmt.Sprintf("https://github.com/alpinelinux/aports/archive/%s.tar.gz", r.Commit)
}

// aportsRecipes covers every Alpine origin whose binaries or libraries are
// shipped in the VM assets (including libseccomp statically linked into
// buildkit-runc and ca-certificates-bundle from the buildkit image).
var aportsRecipes = []aportsRecipe{
	{
		Origin:         "linux-lts",
		Version:        "6.18.64-r0",
		License:        "GPL-2.0-only",
		Commit:         "d65a2d332a8ba2153c1c732411ddd44f8dd2beb8",
		APKBUILDSHA256: "e77f914a3d7a7e3511571a95bca67c55fc720a68e9d3ec0b8ee396d5bc9af644",
		UpstreamSources: []upstreamSource{
			{
				Filename: "linux-6.18.tar.xz",
				URL:      "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.18.tar.xz",
				SHA512:   "88599ffdec96d150c1feb9b261ba93bb0301a9d0e1ad6bef7aeab1f5372cbfc57d8b43c7e902bd8f76921d1dbd8189663c142ea869e51d0e2b483b150ee00fe0",
			},
			{
				Filename: "patch-6.18.64.patch.xz",
				URL:      "https://cdn.kernel.org/pub/linux/kernel/v6.x/patch-6.18.64.xz",
				SHA512:   "5bb845a5c20b1b4b5136f1f136047314c38e618e2cacfc1eb4b79a9f15654b38c7aa5952e7c197650ebbb373c0b84ad712ba30c0d0ede40b907146b6edb68e61",
			},
		},
	},
	{
		Origin:         "busybox",
		Version:        "1.37.0-r31",
		License:        "GPL-2.0-only",
		Commit:         "c3ef5d10e6ef6528852c51f0564963e2f8c1be19",
		APKBUILDSHA256: "98f019f62291aff63cb798f046fb59dfd43340885de63ca2f127651fc21d0450",
		UpstreamSources: []upstreamSource{
			{
				Filename: "busybox-1.37.0.tar.bz2",
				URL:      "https://busybox.net/downloads/busybox-1.37.0.tar.bz2",
				SHA512:   "ad8fd06f082699774f990a53d7a73b189ed404fe0a2166aff13eae4d9d8ee5c9239493befe949c98801fe7897520dbff3ed0224faa7205854ce4fa975e18467e",
			},
		},
	},
	{
		Origin:         "e2fsprogs",
		Version:        "1.47.4-r0",
		License:        "GPL-2.0-or-later AND LGPL-2.0-or-later AND BSD-3-Clause AND MIT",
		Commit:         "8683a6972cb245fbbdb535dad6435a9930bb9374",
		APKBUILDSHA256: "fdeebc52e727e071462ef6616e1d24064fb94a9ed4df88e0b16a3e77d1baa5f3",
		UpstreamSources: []upstreamSource{
			{
				Filename: "e2fsprogs-1.47.4.tar.gz",
				URL:      "https://github.com/tytso/e2fsprogs/archive/v1.47.4.tar.gz",
				SHA512:   "923c1ba52cf48557e190ee936be47ff3e43e4798950499e3c663f0f077470caefbb69ee845ebe1f969b3e7c1bb0c22e507188f1daca24649c54c5843c835b2f9",
			},
		},
	},
	{
		Origin:         "util-linux",
		Version:        "2.42.3-r1",
		License:        "GPL-2.0-or-later AND LGPL-2.1-or-later AND BSD-3-Clause",
		Commit:         "d98c55af59055e6ca60fbe36e171546918709965",
		APKBUILDSHA256: "5347f9d386c8684e7f5cc160ae8595c5c3d3cfedcfdc758f5e022a46e560b944",
		UpstreamSources: []upstreamSource{
			{
				Filename: "util-linux-2.42.3.tar.xz",
				URL:      "https://www.kernel.org/pub/linux/utils/util-linux/v2.42/util-linux-2.42.3.tar.xz",
				SHA512:   "2a307943d4fbb34ac02131e10ff7260f181dd706b21e68a891d25d6eb30476a612bcc19b674bb2256283c9c716146e0d79909f37324efc9aa9d45a44e0d58988",
			},
		},
	},
	{
		Origin:         "libeconf",
		Version:        "0.8.3-r0",
		License:        "MIT",
		Commit:         "62db33049768522f381410614d59c7d732f72c3a",
		APKBUILDSHA256: "b54a5cbbccacc5bd6df56ae141e27fc7ca9ee08accc4c01d9420cc81ba9e8fba",
		UpstreamSources: []upstreamSource{
			{
				Filename: "libeconf-0.8.3.tar.gz",
				URL:      "https://github.com/openSUSE/libeconf/archive/refs/tags/v0.8.3.tar.gz",
				SHA512:   "3ca9bfd85f0a6d7e1ff963257d0e427cf6aec2df2f8a8178b7ade7b2122f5321686d75b7eabe9c8ebe5b6414a32d2e22c06a760ac296285a950c2ed24b792deb",
			},
		},
	},
	{
		Origin:         "musl",
		Version:        "1.2.6-r3",
		License:        "MIT",
		Commit:         "8f79d39391b46d6dbf1800fb68b8212a21d5ee2e",
		APKBUILDSHA256: "22c5adb3f3b68ab84e5ff7d20df68447063d1ecb5c7b39afd5a04a1dfd444a35",
		UpstreamSources: []upstreamSource{
			{
				Filename: "musl-1.2.6.tar.gz",
				URL:      "https://musl.libc.org/releases/musl-1.2.6.tar.gz",
				SHA512:   "1adad96eddb3a2eb0cacb3e363b0046568925fcdd75cf8b0503f2139df1f693d64730779ca0ce8131b7624ab2d37f4247bb1d3393c523de6e30d2b1d7732555c",
			},
		},
	},
	{
		Origin:         "libseccomp",
		Version:        "2.6.1-r0",
		License:        "LGPL-2.1-or-later",
		Commit:         "b32d4ad58620f4ddd96253d62520ae7d07faf477",
		APKBUILDSHA256: "64cb585ea9619879ac3cc4246db0e0291ae961ef887b75369d86390a3fe02ea4",
		UpstreamSources: []upstreamSource{
			{
				Filename: "libseccomp-2.6.1.tar.gz",
				URL:      "https://github.com/seccomp/libseccomp/releases/download/v2.6.1/libseccomp-2.6.1.tar.gz",
				SHA512:   "468404874ee013177f048611a4fefef87f4ee01b83d991f8bf9026c8fdb8506badd613e0355ad8222c5a7f51162b6829de6d72fc53222a56e0e123d5e85f73c4",
			},
		},
	},
	{
		Origin:         "ca-certificates",
		Version:        "20261020-r0",
		License:        "MPL-2.0 AND MIT",
		Commit:         "ac857c298f6fd212498ce76bb87836d940cdba2d",
		APKBUILDSHA256: "7b35fb8e11790105578f47d57a3b22831227b0d8ad7d064e575bad146023f3da",
		UpstreamSources: []upstreamSource{
			{
				Filename: "ca-certificates-20261020.tar.bz2",
				URL:      "https://gitlab.alpinelinux.org/alpine/ca-certificates/-/archive/20261020/ca-certificates-20261020.tar.bz2",
				Mirrors:  []string{alpineDistfiles + "ca-certificates-20261020.tar.bz2"},
				SHA512:   "d28eb01751b1a914ffb5d8cb99e662f0a35a95148c21dcf98de1ab3108b54db9714a99472d07715023867f4f76a21f2a79a2bc1c7e9876ce5167fdd865ad2dd7",
			},
		},
	},
}

// layerPin pins one layer of the moby/buildkit linux/arm64 OCI manifest.
type layerPin struct {
	Digest  string
	Size    int64
	Extract bool // true when the layer contains files we extract
}

// buildkitPin pins moby/buildkit by index digest, arm64 manifest digest,
// every layer digest, and the exact upstream commits for BuildKit, runc,
// libseccomp, and the Mozilla CA bundle shipped in its layers.
var buildkitPin = struct {
	Registry       string
	AuthURL        string
	Repo           string
	Tag            string
	Commit         string
	License        string
	UpstreamURL    string
	RuncVersion    string
	RuncCommit     string
	RuncLicense    string
	RuncURL        string
	IndexDigest    string
	ManifestDigest string
	Layers         []layerPin
}{
	Registry:       "https://registry-1.docker.io",
	AuthURL:        "https://auth.docker.io/token?service=registry.docker.io&scope=repository:moby/buildkit:pull",
	Repo:           "moby/buildkit",
	Tag:            "v0.34.2",
	Commit:         "8c579433824224dc64a158cf8734495b6e2b186a",
	License:        "Apache-2.0",
	UpstreamURL:    "https://github.com/moby/buildkit/tree/8c579433824224dc64a158cf8734495b6e2b186a",
	RuncVersion:    "v1.5.2",
	RuncCommit:     "29dd3dc2b13b4123162e5fe132504bb4b15569f1",
	RuncLicense:    "Apache-2.0",
	RuncURL:        "https://github.com/opencontainers/runc/tree/29dd3dc2b13b4123162e5fe132504bb4b15569f1",
	IndexDigest:    "sha256:c5f8862f52499773af85bc07695b2379ef08e8002352b0df2c4864c91d78dabe",
	ManifestDigest: "sha256:92f93f25966c544549b88144406ce8cd9dcdf5df128250711031a4acd9149a8e",
	Layers: []layerPin{
		{Digest: "sha256:197a036d7c646d3e744af55598877c355ab789d97466837b69296bed05ca0d5f", Size: 4189494, Extract: true},
		{Digest: "sha256:259bfef660747aacb3589fcf23bcf672aa348e6dd0e4c6b26a0e72e91225d368", Size: 12021269},
		{Digest: "sha256:3457cd54502a2ad6d2ed70f9cbe7ca1fb2cd84a7b3a6d18473b0c345cb3a5c33", Size: 940},
		{Digest: "sha256:618b6aac0c4b87be7d0ffa0111bfa69437af51bc9d8c00bb52732dd569bfc9e3", Size: 170},
		{Digest: "sha256:79efd2259b608b27c2cf8d91a575957af6512e588bad3a7215295d06ed585bbf", Size: 86522151, Extract: true},
		{Digest: "sha256:600af964d8b670aa82c5e5605d2ea3c23f082a3fda99f4e46449caac213b4ac8", Size: 610},
		{Digest: "sha256:ab055a6ca909f0fd97fdb07433be90bad0c13c0f4aac2fd5d1e90a5ee9331309", Size: 266},
	},
}

// buildkitWant maps paths inside the buildkit image layers to their
// relative paths under <out>/buildkit/.
var buildkitWant = map[string]struct {
	Rel  string
	Mode uint32
}{
	"usr/bin/buildkitd":                 {Rel: "bin/buildkitd", Mode: 0o755},
	"usr/bin/buildkit-runc":             {Rel: "bin/buildkit-runc", Mode: 0o755},
	"usr/bin/buildctl":                  {Rel: "bin/buildctl", Mode: 0o755},
	"etc/ssl/certs/ca-certificates.crt": {Rel: "certs/ca-certificates.crt", Mode: 0o644},
}

// requiredModules are the kernel modules loaded from the initramfs (plus
// their dependencies resolved from modules.dep).
var requiredModules = []string{
	"fuse", "virtiofs",
	"binfmt_misc",
	"vsock", "vmw_vsock_virtio_transport",
	"virtio_rng", "virtio_balloon",
	"virtio_net", "af_packet",
	"overlay", "veth",
	"virtio_blk",
	"ext4",
}

func recipeForOrigin(origin string) (aportsRecipe, bool) {
	for _, r := range aportsRecipes {
		if r.Origin == origin {
			return r, true
		}
	}
	return aportsRecipe{}, false
}
