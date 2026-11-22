package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCPIODeterminism(t *testing.T) {
	build := func(reverse bool) []byte {
		var c cpioBuilder
		if !reverse {
			c.AddDir("bin", 0o755)
			c.AddDir("lib", 0o755)
			c.AddDir("lib/modules", 0o755)
			c.AddCharDev("dev/console", 0o600, 5, 1)
			c.AddFile("bin/busybox", []byte("busybox"), 0o755)
			c.AddFile("lib/modules/ext4.ko", []byte("ext4mod"), 0o644)
			c.AddSymlink("sbin/mkfs.ext4", "mke2fs")
		} else {
			c.AddSymlink("sbin/mkfs.ext4", "mke2fs")
			c.AddFile("lib/modules/ext4.ko", []byte("ext4mod"), 0o644)
			c.AddFile("bin/busybox", []byte("busybox"), 0o755)
			c.AddCharDev("dev/console", 0o600, 5, 1)
			c.AddDir("lib/modules", 0o755)
			c.AddDir("lib", 0o755)
			c.AddDir("bin", 0o755)
		}
		raw, err := c.Bytes()
		if err != nil {
			t.Fatalf("cpio bytes: %v", err)
		}
		gz, err := gzipDeterministic(raw)
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		return gz
	}

	a := build(false)
	b := build(true)
	if !bytes.Equal(a, b) {
		t.Fatalf("gzipped cpio differed across insertion orders (%d vs %d bytes)", len(a), len(b))
	}

	// Verify gzip header has zero mtime and no filename.
	r, err := gzip.NewReader(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer r.Close()
	if r.Name != "" || !r.ModTime.IsZero() || r.OS != 255 {
		t.Fatalf("non-deterministic gzip header: name=%q mtime=%v os=%d", r.Name, r.ModTime, r.OS)
	}

	// Verify parent dir precedes child and TRAILER!!! is present.
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read cpio: %v", err)
	}
	iLib := bytes.Index(plain, []byte("lib\x00"))
	iMod := bytes.Index(plain, []byte("lib/modules\x00"))
	iKo := bytes.Index(plain, []byte("lib/modules/ext4.ko\x00"))
	iTr := bytes.Index(plain, []byte("TRAILER!!!\x00"))
	if !(0 < iLib && iLib < iMod && iMod < iKo && iKo < iTr) {
		t.Fatalf("unexpected cpio order: lib=%d mod=%d ko=%d trailer=%d", iLib, iMod, iKo, iTr)
	}
}

func TestCPIORejectsDuplicate(t *testing.T) {
	var c cpioBuilder
	c.AddFile("etc/a", []byte("1"), 0o644)
	c.AddFile("etc/a", []byte("2"), 0o644)
	if _, err := c.Bytes(); err == nil {
		t.Fatalf("expected error on duplicate entry")
	}
}

func TestTarGzDeterminism(t *testing.T) {
	makeTree := func(mtime time.Time) (string, int64, []byte) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "buildkit", "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]struct {
			data []byte
			mode os.FileMode
		}{
			"Image":                  {[]byte("kernel"), 0o644},
			"buildkit/bin/buildkitd": {[]byte("#!/bin/sh\n"), 0o755},
			"manifest.json":          {[]byte("{}\n"), 0o644},
		}
		for rel, f := range files {
			full := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.WriteFile(full, f.data, f.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(full, f.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(full, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		out := filepath.Join(t.TempDir(), "assets.tar.gz")
		sha, size, err := writeDeterministicTarGz(dir, out)
		if err != nil {
			t.Fatalf("writeDeterministicTarGz: %v", err)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return sha, size, raw
	}

	sha1, size1, raw1 := makeTree(time.Unix(1000, 0))
	sha2, size2, raw2 := makeTree(time.Unix(2000000, 0))
	if sha1 != sha2 || size1 != size2 || !bytes.Equal(raw1, raw2) {
		t.Fatalf("tar.gz not deterministic: (%s, %d) vs (%s, %d)", sha1, size1, sha2, size2)
	}

	// Inspect tar headers.
	gz, err := gzip.NewReader(bytes.NewReader(raw1))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 || !hdr.ModTime.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("non-zero owner/mtime on %s: uid=%d gid=%d mtime=%v", hdr.Name, hdr.Uid, hdr.Gid, hdr.ModTime)
		}
		if strings.HasPrefix(hdr.Name, "/") || strings.Contains(hdr.Name, "..") {
			t.Fatalf("unsafe tar path: %s", hdr.Name)
		}
		names = append(names, hdr.Name)
	}
	if len(names) != 5 { // Image, buildkit/, buildkit/bin/, buildkit/bin/buildkitd, manifest.json
		t.Fatalf("unexpected tar entries: %v", names)
	}
}

func TestUnwrapKernel(t *testing.T) {
	fakeImage := make([]byte, 128)
	copy(fakeImage[56:60], "ARM\x64")
	gzPayload, err := gzipDeterministic(fakeImage)
	if err != nil {
		t.Fatal(err)
	}

	// 1. EFI zboot container.
	const payloadOff = 64
	zboot := make([]byte, payloadOff+len(gzPayload))
	copy(zboot[0:2], "MZ")
	copy(zboot[4:8], "zimg")
	binary.LittleEndian.PutUint32(zboot[8:12], payloadOff)
	binary.LittleEndian.PutUint32(zboot[12:16], uint32(len(gzPayload)))
	copy(zboot[24:32], "gzip")
	copy(zboot[payloadOff:], gzPayload)

	got, err := unwrapKernel(zboot)
	if err != nil || !bytes.Equal(got, fakeImage) {
		t.Fatalf("unwrap zboot: err=%v equal=%v", err, bytes.Equal(got, fakeImage))
	}

	// 2. Raw gzipped Image.
	got, err = unwrapKernel(gzPayload)
	if err != nil || !bytes.Equal(got, fakeImage) {
		t.Fatalf("unwrap gzip: err=%v equal=%v", err, bytes.Equal(got, fakeImage))
	}

	// 3. Invalid magic fails.
	if _, err := unwrapKernel([]byte("not a kernel")); err == nil {
		t.Fatalf("expected error for invalid kernel")
	}
}

func TestResolveModules(t *testing.T) {
	dep := strings.Join([]string{
		"kernel/fs/ext4/ext4.ko.gz: kernel/lib/crc/crc16.ko.gz kernel/fs/mbcache.ko.gz kernel/fs/jbd2/jbd2.ko.gz",
		"kernel/fs/jbd2/jbd2.ko.gz:",
		"kernel/fs/mbcache.ko.gz:",
		"kernel/lib/crc/crc16.ko.gz:",
		"kernel/drivers/char/hw_random/rng-core.ko.gz:",
		"kernel/drivers/char/hw_random/virtio-rng.ko.gz: kernel/drivers/char/hw_random/rng-core.ko.gz",
	}, "\n")

	order, err := resolveModules(dep, []string{"virtio_rng", "ext4"})
	if err != nil {
		t.Fatalf("resolveModules: %v", err)
	}
	want := []string{
		"kernel/drivers/char/hw_random/rng-core.ko.gz",
		"kernel/drivers/char/hw_random/virtio-rng.ko.gz",
		"kernel/lib/crc/crc16.ko.gz",
		"kernel/fs/mbcache.ko.gz",
		"kernel/fs/jbd2/jbd2.ko.gz",
		"kernel/fs/ext4/ext4.ko.gz",
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", order, want)
	}

	if _, err := resolveModules(dep, []string{"no_such_mod"}); err == nil {
		t.Fatalf("expected error on missing module")
	}
}

func TestExtractAPKData(t *testing.T) {
	makeTarGz := func(files map[string]string) []byte {
		var tb bytes.Buffer
		tw := tar.NewWriter(&tb)
		for name, body := range files {
			_ = tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     name,
				Size:     int64(len(body)),
				Mode:     0o755,
			})
			_, _ = tw.Write([]byte(body))
		}
		_ = tw.Close()
		gz, err := gzipDeterministic(tb.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return gz
	}

	// Simulate Alpine's 3-stream APK: signature + control + data.
	var apk bytes.Buffer
	apk.Write(makeTarGz(map[string]string{".SIGN.RSA.key": "sig"}))
	apk.Write(makeTarGz(map[string]string{".PKGINFO": "pkgname=demo\n"}))
	apk.Write(makeTarGz(map[string]string{"sbin/mke2fs": "ELFDATA"}))

	apkPath := filepath.Join(t.TempDir(), "demo.apk")
	if err := os.WriteFile(apkPath, apk.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := extractAPKData(apkPath)
	if err != nil {
		t.Fatalf("extractAPKData: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "sbin/mke2fs" || string(entries[0].Data) != "ELFDATA" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestDownloadPinnedVerification(t *testing.T) {
	payload := []byte("pinned-package-bytes")
	wantSHA := sha256Bytes(payload)
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/ok.apk":
			_, _ = w.Write(payload)
		case "/missing.apk":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "ok.apk")
	ctx := context.Background()

	// 1. Good download succeeds.
	if err := downloadPinned(ctx, srv.Client(), srv.URL+"/ok.apk", cachePath, wantSHA, nil, true); err != nil {
		t.Fatalf("downloadPinned: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1", hits.Load())
	}

	// 2. Second call uses cache without hitting network.
	if err := downloadPinned(ctx, srv.Client(), srv.URL+"/ok.apk", cachePath, wantSHA, nil, true); err != nil {
		t.Fatalf("cached downloadPinned: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected cache hit, got hits=%d", hits.Load())
	}

	// 3. Bad hash fails and removes partial file.
	badPath := filepath.Join(t.TempDir(), "bad.apk")
	err := downloadPinned(ctx, srv.Client(), srv.URL+"/ok.apk", badPath, strings.Repeat("0", 64), nil, true)
	if !errors.Is(err, errHashMismatch) {
		t.Fatalf("want hash mismatch, got %v", err)
	}
	if _, statErr := os.Stat(badPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("bad file should not exist")
	}

	// 4. Alpine 404 suggests bumping pins.
	missPath := filepath.Join(t.TempDir(), "missing.apk")
	err = downloadPinned(ctx, srv.Client(), srv.URL+"/missing.apk", missPath, wantSHA, nil, true)
	if !errors.Is(err, errNotFound) || !strings.Contains(err.Error(), "bump the pins") {
		t.Fatalf("want alpine 404 bump hint, got %v", err)
	}
}

func TestSanitizedGoEnv(t *testing.T) {
	polluted := []string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"GOOS=darwin",
		"GOARCH=amd64",
		"GOARM64=v9.0",
		"GOAMD64=v4",
		"CGO_ENABLED=1",
		"GOWORK=/tmp/other.work",
		"GOFLAGS=-race -tags=foo",
		"GOTOOLCHAIN=auto",
		"GOEXPERIMENT=arenas",
	}
	got := sanitizedGoEnv(polluted)
	kv := make(map[string]string, len(got))
	for _, entry := range got {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", entry)
		}
		if _, dup := kv[k]; dup {
			t.Fatalf("duplicate env key %q", k)
		}
		kv[k] = v
	}

	want := map[string]string{
		"PATH":         "/usr/bin",
		"HOME":         "/home/user",
		"GOOS":         "linux",
		"GOARCH":       "arm64",
		"GOARM64":      "v8.0",
		"CGO_ENABLED":  "0",
		"GOWORK":       "off",
		"GOFLAGS":      "",
		"GOTOOLCHAIN":  "local",
		"GOEXPERIMENT": "",
	}
	for k, w := range want {
		if kv[k] != w {
			t.Errorf("env %s = %q, want %q", k, kv[k], w)
		}
	}
	if _, has := kv["GOAMD64"]; has {
		t.Errorf("GOAMD64 should be stripped, got %q", kv["GOAMD64"])
	}
}

func TestParseAPKBUILDSha512Sums(t *testing.T) {
	sample := `
pkgname=demo
sha512sums="
aaaa1111  demo-1.0.tar.gz
bbbb2222  fix.patch
"
`
	entries, err := parseAPKBUILDSha512Sums(sample)
	if err != nil {
		t.Fatalf("parseAPKBUILDSha512Sums: %v", err)
	}
	if len(entries) != 2 || entries[0].Filename != "demo-1.0.tar.gz" || entries[0].SHA512 != "aaaa1111" ||
		entries[1].Filename != "fix.patch" || entries[1].SHA512 != "bbbb2222" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestFormatSourcesCompliance(t *testing.T) {
	txt := formatSources(strings.Repeat("a", 64))
	if strings.Contains(txt, "3.24-stable") {
		t.Fatalf("SOURCES must pin immutable aports commits, not moving branch 3.24-stable")
	}
	for _, want := range []string{
		"d65a2d332a8ba2153c1c732411ddd44f8dd2beb8", // linux-lts immutable commit
		"blkid 2.42.3-r1\n  license:  GPL-2.0-or-later",
		"opencontainers/runc v1.5.2",
		"libseccomp 2.6.1-r0 (statically linked into buildkit-runc)",
		"LGPL-2.1-or-later",
		"Mozilla CA bundle",
		"MPL-2.0",
		"LICENSES/",
		"gvx-vm-assets-v1-sources.tar.gz",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("SOURCES missing %q", want)
		}
	}
}

func TestDownloadPinnedSHA512(t *testing.T) {
	payload := []byte("upstream-source-tarball")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	tmp := filepath.Join(t.TempDir(), "src.tar.gz")
	// Compute expected sha512 using helper.
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := sha512File(tmp)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(tmp)

	ctx := context.Background()
	if err := downloadPinnedSHA512(ctx, srv.Client(), srv.URL+"/src.tar.gz", tmp, want); err != nil {
		t.Fatalf("downloadPinnedSHA512: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := downloadPinnedSHA512(ctx, srv.Client(), srv.URL+"/src.tar.gz", bad, strings.Repeat("0", 128)); !errors.Is(err, errHashMismatch) {
		t.Fatalf("want hash mismatch, got %v", err)
	}
}
