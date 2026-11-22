package vmassets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func makeFixtureDir(t *testing.T, ver string, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	m := Manifest{
		Version: ver,
		Components: []Component{
			{Name: "linux-virt", Version: "6.18.64-r0", Source: "https://example.invalid/linux-virt.apk", SHA256: sha256Hex(files[KernelFile])},
		},
		Files: map[string]string{},
	}
	for rel, data := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
		m.Files[rel] = sha256Hex(data)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     []byte
	linkname string
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			if tf == tar.TypeDir {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeValidArchive(t *testing.T, ver string) []byte {
	t.Helper()
	kernel := []byte("fake-arm64-kernel-image")
	initrd := []byte("fake-initramfs-gzip")
	bk := []byte("#!/bin/sh\necho buildkitd\n")
	runc := []byte("#!/bin/sh\necho buildkit-runc\n")
	m := Manifest{
		Version: ver,
		Files: map[string]string{
			KernelFile:                   sha256Hex(kernel),
			InitrdFile:                   sha256Hex(initrd),
			"buildkit/bin/buildkitd":     sha256Hex(bk),
			"buildkit/bin/buildkit-runc": sha256Hex(runc),
		},
	}
	mRaw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return makeTarGz(t, []tarEntry{
		{name: "./", typeflag: tar.TypeDir},
		{name: ManifestFile, body: mRaw},
		{name: KernelFile, body: kernel},
		{name: InitrdFile, body: initrd},
		{name: "buildkit", typeflag: tar.TypeDir},
		{name: "buildkit/bin", typeflag: tar.TypeDir},
		{name: "buildkit/bin/buildkitd", mode: 0o755, body: bk},
		{name: "buildkit/bin/buildkit-runc", mode: 0o755, body: runc},
	})
}

func withTestRelease(t *testing.T, url, sha string, size int64) {
	t.Helper()
	oldURL, oldSHA, oldSize := releaseURL, archiveSHA256, archiveSize
	releaseURL, archiveSHA256, archiveSize = url, sha, size
	t.Cleanup(func() {
		releaseURL, archiveSHA256, archiveSize = oldURL, oldSHA, oldSize
	})
}

func TestAssetsNotPublished(t *testing.T) {
	t.Setenv(EnvOverride, "")
	withTestRelease(t, ReleaseURL, "", 0)

	_, err := Resolve(context.Background(), Options{CacheDir: t.TempDir()})
	if !errors.Is(err, ErrNotPublished) {
		t.Fatalf("Resolve without published sha = %v, want ErrNotPublished", err)
	}
}

func TestAssetsOverrideAndVerify(t *testing.T) {
	files := map[string][]byte{
		KernelFile:                   []byte("kernel-bytes"),
		InitrdFile:                   []byte("initrd-bytes"),
		"buildkit/bin/buildkitd":     []byte("bk-bytes"),
		"buildkit/bin/buildkit-runc": []byte("runc-bytes"),
	}
	dir := makeFixtureDir(t, Version, files)

	// Explicit Options.Override
	d, err := Resolve(context.Background(), Options{Override: dir})
	if err != nil {
		t.Fatalf("Resolve override: %v", err)
	}
	if !d.Override || d.Path != dir {
		t.Fatalf("Resolve override dir = %+v, want Override=true Path=%s", d, dir)
	}
	if d.Kernel() != filepath.Join(dir, KernelFile) || d.Initrd() != filepath.Join(dir, InitrdFile) {
		t.Fatalf("unexpected Kernel/Initrd paths: %s %s", d.Kernel(), d.Initrd())
	}

	// Env override
	t.Setenv(EnvOverride, dir)
	d2, err := Resolve(context.Background(), Options{CacheDir: t.TempDir()})
	if err != nil || !d2.Override || d2.Path != dir {
		t.Fatalf("Resolve env override = (%+v, %v)", d2, err)
	}

	// Corrupt a file -> Verify must fail.
	if err := os.WriteFile(filepath.Join(dir, KernelFile), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Verify corrupted = %v, want checksum mismatch", err)
	}

	// Symlink in asset dir -> Verify must reject.
	symDir := makeFixtureDir(t, Version, files)
	_ = os.Remove(filepath.Join(symDir, InitrdFile))
	if err := os.Symlink(filepath.Join(symDir, KernelFile), filepath.Join(symDir, InitrdFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(symDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Verify symlink = %v, want symlink error", err)
	}

	// Path traversal in manifest -> Verify must reject.
	travDir := t.TempDir()
	badM := Manifest{
		Version: Version,
		Files: map[string]string{
			KernelFile:                   sha256Hex([]byte("k")),
			InitrdFile:                   sha256Hex([]byte("i")),
			"buildkit/bin/buildkitd":     sha256Hex([]byte("b")),
			"buildkit/bin/buildkit-runc": sha256Hex([]byte("r")),
			"../escape":                  sha256Hex([]byte("x")),
		},
	}
	b, _ := json.Marshal(badM)
	_ = os.WriteFile(filepath.Join(travDir, ManifestFile), b, 0o644)
	_ = os.WriteFile(filepath.Join(travDir, KernelFile), []byte("k"), 0o644)
	_ = os.WriteFile(filepath.Join(travDir, InitrdFile), []byte("i"), 0o644)
	_ = os.MkdirAll(filepath.Join(travDir, "buildkit", "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(travDir, "buildkit", "bin", "buildkitd"), []byte("b"), 0o755)
	_ = os.WriteFile(filepath.Join(travDir, "buildkit", "bin", "buildkit-runc"), []byte("r"), 0o755)
	if _, err := Verify(travDir); err == nil {
		t.Fatalf("Verify traversal manifest succeeded, want error")
	}

	// Missing required buildkit binaries -> Verify must reject.
	for _, missing := range []string{"buildkit/bin/buildkitd", "buildkit/bin/buildkit-runc"} {
		subset := map[string][]byte{
			KernelFile:                   []byte("k"),
			InitrdFile:                   []byte("i"),
			"buildkit/bin/buildkitd":     []byte("b"),
			"buildkit/bin/buildkit-runc": []byte("r"),
		}
		delete(subset, missing)
		missDir := makeFixtureDir(t, Version, subset)
		if _, err := Verify(missDir); err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("Verify missing %s = %v, want error naming %s", missing, err, missing)
		}
	}

	// Unlisted file in asset dir -> Verify must reject.
	extraDir := makeFixtureDir(t, Version, files)
	if err := os.WriteFile(filepath.Join(extraDir, "buildkit", "sneaky"), []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(extraDir); err == nil || !strings.Contains(err.Error(), "unlisted file") {
		t.Fatalf("Verify with unlisted file = %v, want unlisted file error", err)
	}
}

func TestAssetsResolveDownloadCacheAndCorruptRecovery(t *testing.T) {
	t.Setenv(EnvOverride, "")
	archive := makeValidArchive(t, Version)
	wantSHA := sha256Hex(archive)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	withTestRelease(t, srv.URL+"/"+ArchiveName, wantSHA, int64(len(archive)))
	cacheDir := t.TempDir()

	var progressCalls atomic.Int32
	d, err := Resolve(context.Background(), Options{
		CacheDir: cacheDir,
		Progress: func(done, total int64) {
			progressCalls.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if d.Override {
		t.Fatalf("cached Resolve returned Override=true")
	}
	if hits.Load() != 1 {
		t.Fatalf("HTTP hits = %d, want 1", hits.Load())
	}
	if progressCalls.Load() == 0 {
		t.Errorf("expected Progress callback to fire")
	}
	fi, err := os.Stat(filepath.Join(d.Path, "buildkit", "bin", "buildkitd"))
	if err != nil {
		t.Fatalf("stat unpacked buildkitd: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("buildkitd mode = %o, want executable", fi.Mode().Perm())
	}

	// Second Resolve hits cache with zero HTTP calls.
	d2, err := Resolve(context.Background(), Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if d2.Path != d.Path || hits.Load() != 1 {
		t.Fatalf("second Resolve path=%s hits=%d, want %s hits=1", d2.Path, hits.Load(), d.Path)
	}

	// Corrupt a cached file -> Resolve detects corruption and re-downloads cleanly.
	if err := os.WriteFile(filepath.Join(d.Path, InitrdFile), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3, err := Resolve(context.Background(), Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("re-download Resolve after corruption: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected re-download after corruption: hits=%d, want 2", hits.Load())
	}
	if _, err := Verify(d3.Path); err != nil {
		t.Fatalf("re-downloaded dir failed Verify: %v", err)
	}
}

func TestAssetsResolveSecurityAndPartialDownload(t *testing.T) {
	t.Setenv(EnvOverride, "")

	t.Run("bad_hash_leaves_no_cache", func(t *testing.T) {
		archive := makeValidArchive(t, Version)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(archive)
		}))
		defer srv.Close()

		withTestRelease(t, srv.URL, strings.Repeat("0", 64), int64(len(archive)))
		cacheDir := t.TempDir()
		if _, err := Resolve(context.Background(), Options{CacheDir: cacheDir}); err == nil {
			t.Fatal("Resolve with bad hash succeeded, want error")
		}
		if _, err := os.Stat(filepath.Join(cacheDir, Version)); !os.IsNotExist(err) {
			t.Errorf("failed Resolve left cache directory behind: %v", err)
		}
	})

	t.Run("oversized_archive_rejected", func(t *testing.T) {
		archive := makeValidArchive(t, Version)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(archive)
		}))
		defer srv.Close()

		withTestRelease(t, srv.URL, sha256Hex(archive), int64(len(archive)-10))
		if _, err := Resolve(context.Background(), Options{CacheDir: t.TempDir()}); err == nil {
			t.Fatal("Resolve exceeding size cap succeeded, want error")
		}
	})

	badCases := []struct {
		name    string
		entries []tarEntry
	}{
		{
			name:    "traversal",
			entries: []tarEntry{{name: "../evil", body: []byte("x")}},
		},
		{
			name:    "absolute",
			entries: []tarEntry{{name: "/etc/evil", body: []byte("x")}},
		},
		{
			name:    "symlink",
			entries: []tarEntry{{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
		},
		{
			name:    "hardlink",
			entries: []tarEntry{{name: "hlink", typeflag: tar.TypeLink, linkname: KernelFile}},
		},
		{
			name:    "fifo",
			entries: []tarEntry{{name: "pipe", typeflag: tar.TypeFifo}},
		},
	}
	for _, tc := range badCases {
		t.Run("tar_"+tc.name, func(t *testing.T) {
			archive := makeTarGz(t, tc.entries)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(archive)
			}))
			defer srv.Close()

			withTestRelease(t, srv.URL, sha256Hex(archive), int64(len(archive)))
			cacheDir := t.TempDir()
			if _, err := Resolve(context.Background(), Options{CacheDir: cacheDir}); err == nil {
				t.Fatalf("Resolve accepted malicious tar (%s)", tc.name)
			}
			if _, err := os.Stat(filepath.Join(cacheDir, Version)); !os.IsNotExist(err) {
				t.Errorf("malicious tar (%s) left cache directory behind", tc.name)
			}
		})
	}
}

func TestAssetsResolveConcurrent(t *testing.T) {
	t.Setenv(EnvOverride, "")
	archive := makeValidArchive(t, Version)
	wantSHA := sha256Hex(archive)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	withTestRelease(t, srv.URL, wantSHA, int64(len(archive)))
	cacheDir := t.TempDir()

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	paths := make(chan string, n)
	for range n {
		wg.Go(func() {
			d, err := Resolve(context.Background(), Options{CacheDir: cacheDir})
			if err != nil {
				errs <- err
				return
			}
			paths <- d.Path
		})
	}
	wg.Wait()
	close(errs)
	close(paths)

	for err := range errs {
		t.Fatalf("concurrent Resolve error: %v", err)
	}
	wantPath := filepath.Join(cacheDir, Version)
	for p := range paths {
		if p != wantPath {
			t.Fatalf("concurrent Resolve path = %s, want %s", p, wantPath)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("concurrent Resolve made %d HTTP downloads, want 1", hits.Load())
	}
}
