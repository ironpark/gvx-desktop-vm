// Command mkassets makes the amd64 builder VM's assets, reproducibly, from
// pinned sources: the same inputs give the same bytes on Linux and macOS.
//
//	go run ./guest/mkassets -out DIR [-archive FILE] [-sources-archive FILE] [-cache DIR]
//
// writes the asset directory vmassets describes (manifest.json, Image,
// initramfs.gz, buildkit/…, LICENSES/…, SOURCES) into DIR and, with
// -archive, the release archive (vmassets.ArchiveName) with its SHA-256
// beside it. With -sources-archive, it also writes a corresponding-source
// tarball containing the pinned Alpine APKBUILDs/patches, verified upstream
// source tarballs, license texts, and BuildKit/runc source refs. Every
// download is checked against a digest pinned in pins.go; nothing reads
// Alpine's moving APKINDEX.
//
// See docs/design/amd64-builder.md. Owner: workstream B.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmassets"
)

func main() {
	outDir := flag.String("out", "", "the asset directory to write")
	archivePath := flag.String("archive", "", "also write the release archive here")
	sourcesArchivePath := flag.String("sources-archive", "", "also write the corresponding-source archive here")
	cacheDir := flag.String("cache", "", "where downloads are kept between runs (default: the user cache dir)")
	repoDir := flag.String("repo", "", "repository root (default: auto-detected from go.mod)")
	flag.Parse()

	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "mkassets: -out is required")
		os.Exit(2)
	}
	if err := run(context.Background(), *outDir, *archivePath, *sourcesArchivePath, *cacheDir, *repoDir); err != nil {
		fmt.Fprintf(os.Stderr, "mkassets: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, outDir, archivePath, sourcesArchivePath, cacheDir, repoDir string) error {
	if repoDir == "" {
		var err error
		repoDir, err = findRepoRoot()
		if err != nil {
			return err
		}
	}
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("user cache dir: %w", err)
		}
		cacheDir = filepath.Join(base, "gvx-desktop", "mkassets-cache")
	}

	client := &http.Client{Timeout: 10 * time.Minute}

	// 1. Build the static linux/arm64 guest agent.
	fmt.Println("mkassets: building guest/agent (linux/arm64, CGO_ENABLED=0)")
	agentBytes, err := buildAgent(ctx, repoDir)
	if err != nil {
		return err
	}
	agentSHA := sha256Bytes(agentBytes)
	fmt.Printf("mkassets: agent %d bytes (sha256 %s)\n", len(agentBytes), agentSHA[:16])

	// 2. Download and verify every pinned Alpine package.
	apkPaths := make(map[string]string, len(pinnedAPKs))
	for _, p := range pinnedAPKs {
		dst := filepath.Join(cacheDir, "alpine-"+alpineBranch, p.Filename())
		fmt.Printf("mkassets: fetch %s\n", p.Filename())
		if err := downloadPinned(ctx, client, p.URL(), dst, p.SHA256, nil, true); err != nil {
			return err
		}
		apkPaths[p.Name] = dst
	}

	// 3. Fetch and verify pinned BuildKit binaries and CA bundle.
	fmt.Printf("mkassets: fetch %s:%s (%s)\n", buildkitPin.Repo, buildkitPin.Tag, buildkitPin.ManifestDigest[:19])
	bkFiles, err := fetchBuildkit(ctx, client, cacheDir)
	if err != nil {
		return err
	}

	// 4. Extract kernel and modules from linux-virt.
	fmt.Println("mkassets: extracting kernel and modules")
	kernelEntries, err := extractAPKData(apkPaths["linux-virt"])
	if err != nil {
		return err
	}
	kernelImage, kernelRelease, modOrder, modData, err := extractKernelAndModules(kernelEntries)
	if err != nil {
		return err
	}
	fmt.Printf("mkassets: kernel %s, Image %d bytes, %d modules\n", kernelRelease, len(kernelImage), len(modOrder))

	// 5. Build deterministic initramfs.gz.
	fmt.Println("mkassets: building initramfs.gz")
	initramfsGz, err := buildInitramfs(repoDir, apkPaths, agentBytes, kernelRelease, modOrder, modData)
	if err != nil {
		return err
	}
	fmt.Printf("mkassets: initramfs.gz %d bytes\n", len(initramfsGz))

	// 6. Stage the asset directory cleanly.
	if err := os.RemoveAll(outDir); err != nil {
		return fmt.Errorf("clean out dir: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir out dir: %w", err)
	}

	if err := writeFile(filepath.Join(outDir, vmassets.KernelFile), kernelImage, 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(outDir, vmassets.InitrdFile), initramfsGz, 0o644); err != nil {
		return err
	}
	for srcName, spec := range buildkitWant {
		dst := filepath.Join(outDir, vmassets.BuildkitDir, filepath.FromSlash(spec.Rel))
		if err := writeFile(dst, bkFiles[srcName], os.FileMode(spec.Mode)); err != nil {
			return err
		}
	}
	bkConf, err := os.ReadFile(filepath.Join(repoDir, "guest", "buildkitd.toml"))
	if err != nil {
		return fmt.Errorf("read guest/buildkitd.toml: %w", err)
	}
	if err := writeFile(filepath.Join(outDir, vmassets.BuildkitDir, "buildkitd.toml"), bkConf, 0o644); err != nil {
		return err
	}

	// 7. Copy license/notice texts, write SOURCES and manifest.json.
	if err := copyLicensesDir(repoDir, filepath.Join(outDir, "LICENSES")); err != nil {
		return err
	}
	sourcesText := formatSources(agentSHA)
	if err := writeFile(filepath.Join(outDir, vmassets.SourcesFile), []byte(sourcesText), 0o644); err != nil {
		return err
	}

	manifest, err := buildManifest(outDir, agentSHA)
	if err != nil {
		return err
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestJSON = append(manifestJSON, '\n')
	if err := writeFile(filepath.Join(outDir, vmassets.ManifestFile), manifestJSON, 0o644); err != nil {
		return err
	}
	fmt.Printf("mkassets: wrote asset directory %s (%d files)\n", outDir, len(manifest.Files)+1)

	// 8. Optionally write deterministic release archive and .sha256 sidecar.
	if archivePath != "" {
		sha, size, err := writeDeterministicTarGz(outDir, archivePath)
		if err != nil {
			return fmt.Errorf("write archive: %w", err)
		}
		shaLine := fmt.Sprintf("%s  %s\n", sha, filepath.Base(archivePath))
		if err := writeFile(archivePath+".sha256", []byte(shaLine), 0o644); err != nil {
			return err
		}
		fmt.Printf("mkassets: archive %s (%d bytes, sha256 %s)\n", archivePath, size, sha)
		fmt.Printf("\nPaste into internal/vmassets/vmassets.go:\n")
		fmt.Printf("const ArchiveSHA256 = %q\n", sha)
		fmt.Printf("const ArchiveSize int64 = %d\n", size)
	}

	// 9. Optionally write corresponding-source archive and .sha256 sidecar.
	if sourcesArchivePath != "" {
		sha, size, err := buildSourcesArchive(ctx, client, repoDir, cacheDir, sourcesText, sourcesArchivePath)
		if err != nil {
			return fmt.Errorf("build sources archive: %w", err)
		}
		fmt.Printf("mkassets: sources archive %s (%d bytes, sha256 %s)\n", sourcesArchivePath, size, sha)
	}
	return nil
}

func copyLicensesDir(repoDir, dstDir string) error {
	srcDir := filepath.Join(repoDir, "guest", "licenses")
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read guest/licenses: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("guest/licenses is empty")
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			return fmt.Errorf("read guest/licenses/%s: %w", name, err)
		}
		if err := writeFile(filepath.Join(dstDir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "guest", "agent")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate repository root (go.mod + guest/agent)")
		}
		dir = parent
	}
}

// sanitizedGoEnv strips Go build-affecting overrides from base and sets the
// target cross-compilation flags explicitly so ambient shell variables
// (GOFLAGS, GOARM64, GOAMD64, GOTOOLCHAIN, GOEXPERIMENT, etc.) cannot affect
// the binary output.
func sanitizedGoEnv(base []string) []string {
	drop := map[string]bool{
		"GOOS":         true,
		"GOARCH":       true,
		"GOARM64":      true,
		"GOAMD64":      true,
		"CGO_ENABLED":  true,
		"GOWORK":       true,
		"GOFLAGS":      true,
		"GOTOOLCHAIN":  true,
		"GOEXPERIMENT": true,
	}
	out := make([]string, 0, len(base)+len(drop))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"GOOS=linux",
		"GOARCH=arm64",
		"GOARM64=v8.0",
		"CGO_ENABLED=0",
		"GOWORK=off",
		"GOFLAGS=",
		"GOTOOLCHAIN=local",
		"GOEXPERIMENT=",
	)
	return out
}

func buildAgent(ctx context.Context, repoDir string) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "gvx-mkassets-agent-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	outBin := filepath.Join(tmpDir, "gvx-agent")
	cmd := exec.CommandContext(ctx, "go", "build",
		"-trimpath",
		"-buildvcs=false",
		"-ldflags", "-s -w -buildid=",
		"-o", outBin,
		"./guest/agent",
	)
	cmd.Dir = repoDir
	cmd.Env = sanitizedGoEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("build guest/agent: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(outBin)
}

func extractKernelAndModules(entries []apkEntry) (image []byte, release string, order []string, mods map[string][]byte, err error) {
	var vmlinuz []byte
	var modulesDep string
	var modPrefix string
	byPath := make(map[string][]byte)

	for _, e := range entries {
		if e.Typeflag != 0 && e.Typeflag != '0' {
			continue
		}
		if e.Name == "boot/vmlinuz-virt" {
			vmlinuz = e.Data
			continue
		}
		if strings.HasPrefix(e.Name, "lib/modules/") {
			parts := strings.SplitN(e.Name, "/", 4)
			if len(parts) == 4 {
				release = parts[2]
				modPrefix = "lib/modules/" + release + "/"
				rel := parts[3]
				if rel == "modules.dep" {
					modulesDep = string(e.Data)
				} else if strings.HasSuffix(rel, ".ko") || strings.HasSuffix(rel, ".ko.gz") {
					byPath[rel] = e.Data
				}
			}
		}
	}

	if len(vmlinuz) == 0 {
		return nil, "", nil, nil, fmt.Errorf("linux-virt: boot/vmlinuz-virt not found")
	}
	if release == "" || modulesDep == "" || modPrefix == "" {
		return nil, "", nil, nil, fmt.Errorf("linux-virt: lib/modules/<release>/modules.dep not found")
	}

	image, err = unwrapKernel(vmlinuz)
	if err != nil {
		return nil, "", nil, nil, err
	}

	order, err = resolveModules(modulesDep, requiredModules)
	if err != nil {
		return nil, "", nil, nil, err
	}

	mods = make(map[string][]byte, len(order))
	for _, rel := range order {
		raw, ok := byPath[rel]
		if !ok {
			return nil, "", nil, nil, fmt.Errorf("linux-virt: module %s missing from package", rel)
		}
		if strings.HasSuffix(rel, ".gz") {
			raw, err = gunzipBytes(raw)
			if err != nil {
				return nil, "", nil, nil, fmt.Errorf("linux-virt: gunzip %s: %w", rel, err)
			}
		}
		mods[rel] = raw
	}
	return image, release, order, mods, nil
}

func buildInitramfs(repoDir string, apkPaths map[string]string, agentBytes []byte, kernelRelease string, modOrder []string, modData map[string][]byte) ([]byte, error) {
	var c cpioBuilder

	for _, d := range []string{"bin", "dev", "etc", "lib", "lib/modules", "newroot", "proc", "sbin", "usr", "usr/lib"} {
		c.AddDir(d, 0o755)
	}
	c.AddCharDev("dev/console", 0o600, 5, 1)

	// Guest agent binary.
	c.AddFile("bin/gvx-agent", agentBytes, 0o755)

	// Rootfs scripts from guest/rootfs/.
	rootfsFiles := []struct {
		src  string
		dst  string
		mode uint32
	}{
		{"init", "init", 0o755},
		{"rc", "etc/rc", 0o755},
		{"udhcpc.sh", "etc/udhcpc.sh", 0o755},
		{"inittab", "etc/inittab", 0o644},
	}
	for _, rf := range rootfsFiles {
		b, err := os.ReadFile(filepath.Join(repoDir, "guest", "rootfs", rf.src))
		if err != nil {
			return nil, fmt.Errorf("read guest/rootfs/%s: %w", rf.src, err)
		}
		c.AddFile(rf.dst, b, rf.mode)
	}

	c.AddFile("etc/passwd", []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644)
	c.AddFile("etc/group", []byte("root:x:0:\n"), 0o644)
	c.AddFile("etc/hosts", []byte("127.0.0.1 localhost\n::1 localhost\n"), 0o644)
	c.AddFile("etc/kernel-release", []byte(kernelRelease+"\n"), 0o644)

	// Busybox static.
	if err := addAPKMatches(&c, apkPaths["busybox-static"], map[string]string{
		"bin/busybox.static": "bin/busybox",
	}); err != nil {
		return nil, err
	}

	// e2fsprogs (mke2fs, e2fsck, mke2fs.conf, mkfs.ext4/fsck.ext4 symlinks).
	if err := addAPKMatches(&c, apkPaths["e2fsprogs"], map[string]string{
		"sbin/mke2fs":     "sbin/mke2fs",
		"sbin/e2fsck":     "sbin/e2fsck",
		"sbin/mkfs.ext4":  "sbin/mkfs.ext4",
		"sbin/fsck.ext4":  "sbin/fsck.ext4",
		"etc/mke2fs.conf": "etc/mke2fs.conf",
	}); err != nil {
		return nil, err
	}

	// blkid.
	if err := addAPKMatches(&c, apkPaths["blkid"], map[string]string{
		"sbin/blkid": "sbin/blkid",
	}); err != nil {
		return nil, err
	}

	// musl dynamic loader.
	if err := addAPKMatches(&c, apkPaths["musl"], map[string]string{
		"lib/ld-musl-aarch64.so.1":   "lib/ld-musl-aarch64.so.1",
		"lib/libc.musl-aarch64.so.1": "lib/libc.musl-aarch64.so.1",
	}); err != nil {
		return nil, err
	}

	// Shared libraries needed by mke2fs, e2fsck, and blkid.
	for _, pkg := range []string{"e2fsprogs-libs", "libcom_err", "libblkid", "libuuid", "libeconf"} {
		if err := addAPKPrefix(&c, apkPaths[pkg], "usr/lib/"); err != nil {
			return nil, err
		}
	}

	// Kernel modules and load order list.
	var modNames []string
	for _, rel := range modOrder {
		base := path.Base(rel)
		name := strings.Split(base, ".ko")[0] + ".ko"
		c.AddFile("lib/modules/"+name, modData[rel], 0o644)
		modNames = append(modNames, name)
	}
	c.AddFile("lib/modules/load.list", []byte(strings.Join(modNames, "\n")+"\n"), 0o644)

	rawCPIO, err := c.Bytes()
	if err != nil {
		return nil, err
	}
	return gzipDeterministic(rawCPIO)
}

func addAPKMatches(c *cpioBuilder, apkPath string, want map[string]string) error {
	entries, err := extractAPKData(apkPath)
	if err != nil {
		return err
	}
	found := make(map[string]bool, len(want))
	for _, e := range entries {
		dst, ok := want[e.Name]
		if !ok {
			continue
		}
		found[e.Name] = true
		switch e.Typeflag {
		case '2':
			c.AddSymlink(dst, e.Linkname)
		default:
			mode := uint32(0o644)
			if e.Executable || strings.HasPrefix(dst, "bin/") || strings.HasPrefix(dst, "sbin/") || strings.HasSuffix(dst, ".so.1") {
				mode = 0o755
			}
			c.AddFile(dst, e.Data, mode)
		}
	}
	var missing []string
	for src := range want {
		if !found[src] {
			missing = append(missing, src)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%s: missing expected files: %s", filepath.Base(apkPath), strings.Join(missing, ", "))
	}
	return nil
}

func addAPKPrefix(c *cpioBuilder, apkPath, prefix string) error {
	entries, err := extractAPKData(apkPath)
	if err != nil {
		return err
	}
	count := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name, prefix) {
			continue
		}
		count++
		switch e.Typeflag {
		case '2':
			c.AddSymlink(e.Name, e.Linkname)
		default:
			mode := uint32(0o644)
			if e.Executable || strings.Contains(e.Name, ".so") {
				mode = 0o755
			}
			c.AddFile(e.Name, e.Data, mode)
		}
	}
	if count == 0 {
		return fmt.Errorf("%s: no entries under %s", filepath.Base(apkPath), prefix)
	}
	return nil
}

func buildManifest(outDir, agentSHA string) (vmassets.Manifest, error) {
	components := make([]vmassets.Component, 0, len(pinnedAPKs)+5)
	for _, p := range pinnedAPKs {
		components = append(components, vmassets.Component{
			Name:    p.Name,
			Version: p.Version,
			Source:  p.URL(),
			SHA256:  p.SHA256,
		})
	}
	components = append(components,
		vmassets.Component{
			Name:    "buildkit",
			Version: buildkitPin.Tag,
			Source:  fmt.Sprintf("%s:%s@%s", buildkitPin.Repo, buildkitPin.Tag, buildkitPin.ManifestDigest),
			SHA256:  trimSHA256(buildkitPin.ManifestDigest),
		},
		vmassets.Component{
			Name:    "runc",
			Version: buildkitPin.RuncVersion,
			Source:  buildkitPin.RuncURL,
			SHA256:  buildkitPin.RuncCommit,
		},
	)
	if r, ok := recipeForOrigin("libseccomp"); ok {
		components = append(components, vmassets.Component{
			Name:    "libseccomp",
			Version: r.Version,
			Source:  r.TreeURL(),
			SHA256:  r.APKBUILDSHA256,
		})
	}
	if r, ok := recipeForOrigin("ca-certificates"); ok {
		components = append(components, vmassets.Component{
			Name:    "ca-certificates",
			Version: r.Version,
			Source:  r.TreeURL(),
			SHA256:  r.APKBUILDSHA256,
		})
	}
	components = append(components, vmassets.Component{
		Name:    "agent",
		Version: vmassets.Version,
		Source:  "github.com/ironpark/gvx-desktop-vm/guest/agent",
		SHA256:  agentSHA,
	})

	files := make(map[string]string)
	err := filepath.WalkDir(outDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(outDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == vmassets.ManifestFile {
			return nil
		}
		sum, err := sha256File(p)
		if err != nil {
			return err
		}
		files[rel] = sum
		return nil
	})
	if err != nil {
		return vmassets.Manifest{}, err
	}

	return vmassets.Manifest{
		Version:    vmassets.Version,
		Components: components,
		Files:      files,
	}, nil
}

func formatSources(agentSHA string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "GVX Desktop amd64 builder VM assets — version %s\n", vmassets.Version)
	sb.WriteString("Exact upstream binary inputs, immutable source commits, and corresponding source locations.\n")
	sb.WriteString("This archive includes GPL, LGPL, MPL, Apache-2.0, BSD-3-Clause, and MIT software.\n")
	sb.WriteString("License and notice texts are shipped in LICENSES/.\n")
	fmt.Fprintf(&sb, "Corresponding source archive: gvx-vm-assets-v%s-sources.tar.gz (published on the same release).\n\n", vmassets.Version)

	sb.WriteString("## Alpine Linux packages (branch " + alpineBranch + ", arch aarch64)\n\n")
	for _, p := range pinnedAPKs {
		fmt.Fprintf(&sb, "%s %s\n", p.Name, p.Version)
		fmt.Fprintf(&sb, "  license:  %s\n", p.License)
		fmt.Fprintf(&sb, "  package:  %s\n", p.URL())
		fmt.Fprintf(&sb, "  sha256:   %s\n", p.SHA256)
		fmt.Fprintf(&sb, "  aports:   %s\n", p.AportsURL())
		if r, ok := recipeForOrigin(p.Origin); ok {
			fmt.Fprintf(&sb, "  apkbuild: %s (sha256 %s)\n", r.RawFileURL("APKBUILD"), r.APKBUILDSHA256)
			for _, u := range r.UpstreamSources {
				fmt.Fprintf(&sb, "  source:   %s\n", u.URL)
				fmt.Fprintf(&sb, "  sha512:   %s\n", u.SHA512)
			}
		}
		fmt.Fprintf(&sb, "  upstream: %s\n\n", p.UpstreamURL)
	}

	sb.WriteString("## BuildKit, runc, libseccomp, and Mozilla CA bundle\n\n")
	fmt.Fprintf(&sb, "%s %s (linux/arm64: buildkitd, buildctl)\n", buildkitPin.Repo, buildkitPin.Tag)
	fmt.Fprintf(&sb, "  license:  %s\n", buildkitPin.License)
	fmt.Fprintf(&sb, "  commit:   %s\n", buildkitPin.Commit)
	fmt.Fprintf(&sb, "  index:    %s@%s\n", buildkitPin.Repo, buildkitPin.IndexDigest)
	fmt.Fprintf(&sb, "  manifest: %s@%s\n", buildkitPin.Repo, buildkitPin.ManifestDigest)
	for _, l := range buildkitPin.Layers {
		if l.Extract {
			fmt.Fprintf(&sb, "  layer:    %s (%d bytes)\n", l.Digest, l.Size)
		}
	}
	fmt.Fprintf(&sb, "  upstream: %s\n\n", buildkitPin.UpstreamURL)

	fmt.Fprintf(&sb, "opencontainers/runc %s (shipped as buildkit-runc)\n", buildkitPin.RuncVersion)
	fmt.Fprintf(&sb, "  license:  %s\n", buildkitPin.RuncLicense)
	fmt.Fprintf(&sb, "  commit:   %s\n", buildkitPin.RuncCommit)
	fmt.Fprintf(&sb, "  upstream: %s\n\n", buildkitPin.RuncURL)

	if r, ok := recipeForOrigin("libseccomp"); ok {
		fmt.Fprintf(&sb, "libseccomp %s (statically linked into buildkit-runc)\n", r.Version)
		fmt.Fprintf(&sb, "  license:  %s\n", r.License)
		fmt.Fprintf(&sb, "  aports:   %s\n", r.TreeURL())
		fmt.Fprintf(&sb, "  apkbuild: %s (sha256 %s)\n", r.RawFileURL("APKBUILD"), r.APKBUILDSHA256)
		for _, u := range r.UpstreamSources {
			fmt.Fprintf(&sb, "  source:   %s\n", u.URL)
			fmt.Fprintf(&sb, "  sha512:   %s\n", u.SHA512)
		}
		sb.WriteString("  upstream: https://github.com/seccomp/libseccomp\n\n")
	}

	if r, ok := recipeForOrigin("ca-certificates"); ok {
		fmt.Fprintf(&sb, "Mozilla CA bundle (ca-certificates-bundle %s)\n", r.Version)
		fmt.Fprintf(&sb, "  license:  %s\n", r.License)
		fmt.Fprintf(&sb, "  aports:   %s\n", r.TreeURL())
		fmt.Fprintf(&sb, "  apkbuild: %s (sha256 %s)\n", r.RawFileURL("APKBUILD"), r.APKBUILDSHA256)
		for _, u := range r.UpstreamSources {
			fmt.Fprintf(&sb, "  source:   %s\n", u.URL)
			fmt.Fprintf(&sb, "  sha512:   %s\n", u.SHA512)
		}
		sb.WriteString("  upstream: https://www.mozilla.org/en-US/about/governance/policies/security-group/certs/\n\n")
	}

	sb.WriteString("## GVX guest agent\n\n")
	fmt.Fprintf(&sb, "gvx-agent %s (linux/arm64, static)\n", vmassets.Version)
	sb.WriteString("  license:  Apache-2.0\n")
	sb.WriteString("  source:   https://github.com/ironpark/gvx-desktop (guest/agent)\n")
	fmt.Fprintf(&sb, "  sha256:   %s\n", agentSHA)

	return sb.String()
}

func writeFile(dst string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
