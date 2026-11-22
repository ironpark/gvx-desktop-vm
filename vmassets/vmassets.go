// Package vmassets finds the amd64 builder VM's assets: the kernel, the
// initramfs (busybox, e2fsprogs, the agent, the kernel modules) and
// buildkit's binaries. They are never in the repository: guest/mkassets
// makes them, reproducibly, from pinned sources, a GitHub release of this
// repository (ReleaseTag) carries them, and Resolve downloads them on first
// use into the user's cache, checking every byte against the SHA-256s
// pinned here. For development, a directory mkassets wrote can stand in
// (Options.Override, or $GVX_VM_ASSETS).
//
// The asset directory, as mkassets writes it and the archive holds it:
//
//	manifest.json                    Manifest
//	Image                            the bare arm64 kernel (KernelFile)
//	initramfs.gz                     the guest's root (InitrdFile)
//	buildkit/bin/buildkitd           shared with the guest read-only,
//	buildkit/bin/buildkit-runc       mounted at /mnt/assets
//	buildkit/bin/buildctl
//	buildkit/certs/ca-certificates.crt
//	buildkit/buildkitd.toml
//	SOURCES                          where each component came from
//
// See docs/design/amd64-builder.md. The API (names, types, signatures and
// constants other than the pins) is frozen (architect); the bodies are
// workstream C's, the pins are filled by whoever publishes a release (the
// user), from mkassets' output.
package vmassets

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Version is the asset set's version. The release that carries it is
// ReleaseTag; a new set (a kernel or buildkit bump) is a new Version.
const Version = "1"

// ReleaseTag is the GitHub release of this repository with the archive.
const ReleaseTag = "vm-assets-v" + Version

// ArchiveName is the release asset: a gzipped tar of the asset directory.
const ArchiveName = "gvx-vm-assets-v" + Version + "-arm64.tar.gz"

// ReleaseURL is where ArchiveName is downloaded from.
const ReleaseURL = "https://github.com/ironpark/gvx-desktop-vm/releases/download/" + ReleaseTag + "/" + ArchiveName

// ArchiveSHA256 is the archive's SHA-256, hex. Empty until the first
// release is published: Resolve then needs an override.
const ArchiveSHA256 = ""

// ArchiveSize is the archive's size in bytes, for progress and as a cap on
// the download; zero until published.
const ArchiveSize int64 = 0

// EnvOverride names the environment variable that points at a local asset
// directory, for development.
const EnvOverride = "GVX_VM_ASSETS"

// File names in the asset directory.
const (
	ManifestFile = "manifest.json"
	KernelFile   = "Image"
	InitrdFile   = "initramfs.gz"
	BuildkitDir  = "buildkit"
	SourcesFile  = "SOURCES"
)

// Manifest describes an asset directory. mkassets writes it; Resolve and
// Verify check the files against it.
type Manifest struct {
	// Version is the asset set's (Version above, for a release).
	Version string `json:"version"`
	// Components say what went in, for the Settings page and bug reports.
	Components []Component `json:"components"`
	// Files maps each file's slash-separated path in the directory to its
	// SHA-256, hex. Every file but the manifest itself is listed.
	Files map[string]string `json:"files"`
}

// Component is one thing the assets are made of.
type Component struct {
	Name    string `json:"name"`    // "linux-virt", "busybox-static", "buildkit", "agent", …
	Version string `json:"version"` // "6.18.64-r0", "v0.34.2", …
	Source  string `json:"source"`  // URL or image reference with digest
	SHA256  string `json:"sha256"`  // of what was downloaded
}

// Dir is a verified asset directory.
type Dir struct {
	Path     string
	Manifest Manifest
	// Override reports that Path is a local override, not the cache.
	Override bool
}

// Kernel is the kernel's path.
func (d Dir) Kernel() string { return filepath.Join(d.Path, KernelFile) }

// Initrd is the initramfs' path.
func (d Dir) Initrd() string { return filepath.Join(d.Path, InitrdFile) }

// Options say where Resolve looks.
type Options struct {
	// CacheDir holds downloaded asset sets, one directory per Version.
	// Empty is DefaultCacheDir.
	CacheDir string
	// Override is a local asset directory to use instead; empty is
	// $GVX_VM_ASSETS, and when that is empty too, the cache.
	Override string
	// Progress, when set, is told how the download goes.
	Progress func(done, total int64)
}

// ErrNotPublished is Resolve's error when no release is pinned yet and no
// override is given.
var ErrNotPublished = errors.New("the amd64 builder's VM assets are not published yet; set " + EnvOverride + " to a directory guest/mkassets made")

// Package-level hooks for tests to point at a local HTTP server and archive.
var (
	releaseURL    = ReleaseURL
	archiveSHA256 = ArchiveSHA256
	archiveSize   = ArchiveSize
)

const (
	maxManifestBytes      = 4 << 20
	defaultMaxArchiveSize = 512 << 20
	maxUnpackBytes        = 1 << 30
)

// DefaultCacheDir is <user cache dir>/gvx-desktop/vm-assets.
func DefaultCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gvx-desktop", "vm-assets"), nil
}

// Resolve returns a verified asset directory: the override when there is
// one (verified against its own manifest), else the cached set for
// Version (verified again: a file changed since is downloaded anew), else
// it downloads ReleaseURL, checks ArchiveSHA256, unpacks into a temporary
// directory beside the cache, verifies, and renames it into place. Two
// Resolves at once, in this process or another, end with one directory.
func Resolve(ctx context.Context, opts Options) (Dir, error) {
	override := strings.TrimSpace(opts.Override)
	if override == "" {
		override = strings.TrimSpace(os.Getenv(EnvOverride))
	}
	if override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			override = abs
		}
		m, err := Verify(override)
		if err != nil {
			return Dir{}, fmt.Errorf("verify VM assets override %s: %w", override, err)
		}
		return Dir{Path: override, Manifest: m, Override: true}, nil
	}

	wantSHA := strings.TrimSpace(archiveSHA256)
	if wantSHA == "" {
		return Dir{}, ErrNotPublished
	}

	cacheRoot := strings.TrimSpace(opts.CacheDir)
	if cacheRoot == "" {
		var err error
		cacheRoot, err = DefaultCacheDir()
		if err != nil {
			return Dir{}, fmt.Errorf("locate VM assets cache dir: %w", err)
		}
	}
	if abs, err := filepath.Abs(cacheRoot); err == nil {
		cacheRoot = abs
	}
	targetDir := filepath.Join(cacheRoot, Version)

	// Fast path: already cached and verified.
	if m, err := Verify(targetDir); err == nil && (m.Version == "" || m.Version == Version) {
		return Dir{Path: targetDir, Manifest: m}, nil
	}

	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return Dir{}, fmt.Errorf("create VM assets cache dir: %w", err)
	}

	unlock, err := lockCache(ctx, filepath.Join(cacheRoot, Version+".lock"))
	if err != nil {
		return Dir{}, fmt.Errorf("lock VM assets cache: %w", err)
	}
	defer unlock()

	// Re-check inside the lock in case another Resolve just populated targetDir.
	if m, err := Verify(targetDir); err == nil && (m.Version == "" || m.Version == Version) {
		return Dir{Path: targetDir, Manifest: m}, nil
	}

	tmpArchive, err := os.CreateTemp(cacheRoot, ".download-"+Version+"-*.tar.gz")
	if err != nil {
		return Dir{}, fmt.Errorf("create temp archive: %w", err)
	}
	tmpArchivePath := tmpArchive.Name()
	defer func() {
		_ = tmpArchive.Close()
		_ = os.Remove(tmpArchivePath)
	}()

	limit := archiveSize
	if limit <= 0 {
		limit = defaultMaxArchiveSize
	}
	if err := download(ctx, releaseURL, ArchiveName, archiveSize, wantSHA, tmpArchive, limit, opts.Progress); err != nil {
		return Dir{}, fmt.Errorf("download VM assets: %w", err)
	}
	if _, err := tmpArchive.Seek(0, io.SeekStart); err != nil {
		return Dir{}, fmt.Errorf("rewind VM assets archive: %w", err)
	}

	tmpDir, err := os.MkdirTemp(cacheRoot, ".unpack-"+Version+"-*")
	if err != nil {
		return Dir{}, fmt.Errorf("create unpack dir: %w", err)
	}
	moved := false
	defer func() {
		if !moved {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := unpackTarGz(tmpArchive, tmpDir); err != nil {
		return Dir{}, fmt.Errorf("unpack VM assets: %w", err)
	}

	m, err := Verify(tmpDir)
	if err != nil {
		return Dir{}, fmt.Errorf("verify unpacked VM assets: %w", err)
	}
	if m.Version != "" && m.Version != Version {
		return Dir{}, fmt.Errorf("VM assets manifest version %q does not match expected %q", m.Version, Version)
	}

	if err := replaceDir(tmpDir, targetDir); err != nil {
		return Dir{}, fmt.Errorf("install VM assets: %w", err)
	}
	moved = true
	return Dir{Path: targetDir, Manifest: m}, nil
}

// Verify checks every file of the asset directory dir against its
// manifest and returns the manifest.
func Verify(dir string) (Manifest, error) {
	if strings.TrimSpace(dir) == "" {
		return Manifest{}, errors.New("empty asset directory")
	}
	manifestPath := filepath.Join(dir, ManifestFile)
	fi, err := os.Lstat(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("stat %s: %w", ManifestFile, err)
	}
	if !fi.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("%s is not a regular file", ManifestFile)
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	_ = f.Close()
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", ManifestFile, err)
	}
	if len(raw) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("%s exceeds %d bytes", ManifestFile, maxManifestBytes)
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", ManifestFile, err)
	}
	if len(m.Files) == 0 {
		return Manifest{}, fmt.Errorf("%s lists no files", ManifestFile)
	}
	for _, req := range []string{KernelFile, InitrdFile, "buildkit/bin/buildkitd", "buildkit/bin/buildkit-runc"} {
		if _, ok := m.Files[req]; !ok {
			return Manifest{}, fmt.Errorf("%s is missing required file %s", ManifestFile, req)
		}
	}

	keys := make([]string, 0, len(m.Files))
	for k := range m.Files {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	for _, rel := range keys {
		want := strings.ToLower(strings.TrimSpace(m.Files[rel]))
		if err := validateRelPath(rel); err != nil {
			return Manifest{}, fmt.Errorf("invalid manifest path %q: %w", rel, err)
		}
		if rel == ManifestFile {
			return Manifest{}, fmt.Errorf("%s must not list itself", ManifestFile)
		}
		if !isSHA256Hex(want) {
			return Manifest{}, fmt.Errorf("invalid sha256 for %s", rel)
		}
		full, err := checkNoSymlinkPath(dir, rel)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", rel, err)
		}
		got, err := sha256File(full)
		if err != nil {
			return Manifest{}, fmt.Errorf("hash %s: %w", rel, err)
		}
		if got != want {
			return Manifest{}, fmt.Errorf("checksum mismatch for %s: got %s, want %s", rel, got, want)
		}
	}

	// Reject any unlisted files or symlinks inside the asset directory.
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlinks are not allowed", rel)
		}
		if d.IsDir() {
			return nil
		}
		if rel == ManifestFile {
			return nil
		}
		if _, ok := m.Files[rel]; !ok {
			return fmt.Errorf("unlisted file in asset directory: %s", rel)
		}
		return nil
	}); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func validateRelPath(rel string) error {
	if rel == "" {
		return errors.New("empty path")
	}
	if strings.ContainsRune(rel, '\\') || strings.HasPrefix(rel, "/") {
		return errors.New("path must be relative and slash-separated")
	}
	clean := path.Clean(rel)
	if clean != rel || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("path must be clean and stay within directory")
	}
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// checkNoSymlinkPath verifies that no component along rel inside baseDir is a
// symlink and that the final target is a regular file.
func checkNoSymlinkPath(baseDir, rel string) (string, error) {
	parts := strings.Split(rel, "/")
	cur := baseDir
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", errors.New("symlinks are not allowed")
		}
		if i < len(parts)-1 {
			if !fi.IsDir() {
				return "", errors.New("expected directory")
			}
		} else if !fi.Mode().IsRegular() {
			return "", errors.New("not a regular file")
		}
	}
	return cur, nil
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func unpackTarGz(r io.Reader, dstDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var totalWritten int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		name := hdr.Name
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") || strings.ContainsRune(name, '\\') {
			return fmt.Errorf("archive entry %q has forbidden path", hdr.Name)
		}
		for _, seg := range strings.Split(name, "/") {
			if seg == ".." {
				return fmt.Errorf("archive entry %q escapes target directory", hdr.Name)
			}
		}
		clean := path.Clean(strings.TrimPrefix(name, "./"))
		if clean == "." || clean == "" {
			if hdr.Typeflag == tar.TypeDir {
				continue
			}
			return fmt.Errorf("archive entry %q has invalid root name", hdr.Name)
		}
		if err := validateRelPath(clean); err != nil {
			return fmt.Errorf("archive entry %q: %w", hdr.Name, err)
		}

		target := filepath.Join(dstDir, filepath.FromSlash(clean))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || totalWritten+hdr.Size > maxUnpackBytes {
				return fmt.Errorf("archive exceeds unpack limit of %d bytes", maxUnpackBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			perm := fs.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				perm = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(out, io.LimitReader(tr, maxUnpackBytes-totalWritten+1))
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			totalWritten += n
			if totalWritten > maxUnpackBytes {
				return fmt.Errorf("archive exceeds unpack limit of %d bytes", maxUnpackBytes)
			}
		default:
			return fmt.Errorf("archive entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
}

func replaceDir(srcDir, dstDir string) error {
	if _, err := os.Lstat(dstDir); errors.Is(err, fs.ErrNotExist) {
		return os.Rename(srcDir, dstDir)
	}
	backup := srcDir + ".old"
	if err := os.Rename(dstDir, backup); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Rename(srcDir, dstDir)
	}
	if err := os.Rename(srcDir, dstDir); err != nil {
		_ = os.Rename(backup, dstDir)
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}
