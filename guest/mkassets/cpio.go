package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	cpioIFDIR = 0o040000
	cpioIFCHR = 0o020000
	cpioIFREG = 0o100000
	cpioIFLNK = 0o120000
)

type cpioEntry struct {
	Name  string
	Mode  uint32
	Data  []byte
	Major uint32
	Minor uint32
}

// cpioBuilder builds a deterministic SVR4 "newc" (070701) CPIO archive
// with sorted entries, uid/gid 0, and mtime 0.
type cpioBuilder struct {
	entries []cpioEntry
}

func (b *cpioBuilder) AddDir(name string, mode uint32) {
	b.entries = append(b.entries, cpioEntry{
		Name: cleanArchivePath(name),
		Mode: cpioIFDIR | (mode & 0o7777),
	})
}

func (b *cpioBuilder) AddFile(name string, data []byte, mode uint32) {
	cp := make([]byte, len(data))
	copy(cp, data)
	b.entries = append(b.entries, cpioEntry{
		Name: cleanArchivePath(name),
		Mode: cpioIFREG | (mode & 0o7777),
		Data: cp,
	})
}

func (b *cpioBuilder) AddSymlink(name, target string) {
	b.entries = append(b.entries, cpioEntry{
		Name: cleanArchivePath(name),
		Mode: cpioIFLNK | 0o777,
		Data: []byte(target),
	})
}

func (b *cpioBuilder) AddCharDev(name string, mode, major, minor uint32) {
	b.entries = append(b.entries, cpioEntry{
		Name:  cleanArchivePath(name),
		Mode:  cpioIFCHR | (mode & 0o7777),
		Major: major,
		Minor: minor,
	})
}

func cleanArchivePath(p string) string {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	return p
}

// comparePathParts orders paths by component so parent directories always
// sort before their children, deterministically.
func comparePathParts(a, b string) int {
	pa := strings.Split(a, "/")
	pb := strings.Split(b, "/")
	n := len(pa)
	if len(pb) < n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	if len(pa) < len(pb) {
		return -1
	}
	if len(pa) > len(pb) {
		return 1
	}
	return 0
}

func (b *cpioBuilder) Bytes() ([]byte, error) {
	sorted := make([]cpioEntry, len(b.entries))
	copy(sorted, b.entries)
	sort.Slice(sorted, func(i, j int) bool {
		return comparePathParts(sorted[i].Name, sorted[j].Name) < 0
	})

	var buf bytes.Buffer
	var ino uint32
	for i, e := range sorted {
		if e.Name == "" || e.Name == "." {
			return nil, fmt.Errorf("cpio: invalid entry name %q", e.Name)
		}
		if i > 0 && sorted[i-1].Name == e.Name {
			return nil, fmt.Errorf("cpio: duplicate entry %q", e.Name)
		}
		ino++
		writeCPIOEntry(&buf, ino, e)
	}
	ino++
	writeCPIOEntry(&buf, ino, cpioEntry{Name: "TRAILER!!!"})
	return buf.Bytes(), nil
}

func writeCPIOEntry(buf *bytes.Buffer, ino uint32, e cpioEntry) {
	nameBytes := append([]byte(e.Name), 0)
	fmt.Fprintf(buf, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino,
		e.Mode,
		uint32(0), // uid
		uint32(0), // gid
		uint32(1), // nlink
		uint32(0), // mtime
		uint32(len(e.Data)),
		uint32(0), // devmajor
		uint32(0), // devminor
		e.Major,
		e.Minor,
		uint32(len(nameBytes)),
		uint32(0), // check
	)
	buf.Write(nameBytes)
	pad4(buf)
	buf.Write(e.Data)
	pad4(buf)
}

func pad4(buf *bytes.Buffer) {
	if rem := buf.Len() % 4; rem != 0 {
		var zero [3]byte
		buf.Write(zero[:4-rem])
	}
}

// gzipDeterministic compresses data with a fixed level and zeroed header
// (no filename, zero mtime, platform-neutral OS byte 255).
func gzipDeterministic(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	if err != nil {
		return nil, err
	}
	gz.Header = gzip.Header{OS: 255}
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeDeterministicTarGz packs dir into a reproducible gzipped USTAR
// tarball at archivePath with sorted entries, owner 0, mtime 0, and fixed
// modes. It returns the archive's SHA-256 hex digest and size in bytes.
func writeDeterministicTarGz(dir, archivePath string) (string, int64, error) {
	type item struct {
		rel   string
		full  string
		isDir bool
		exec  bool
		size  int64
	}

	var items []item
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive refuses symlink %s", rel)
		}
		if d.IsDir() {
			items = append(items, item{rel: rel, full: p, isDir: true})
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("archive refuses non-regular file %s", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		items = append(items, item{
			rel:  rel,
			full: p,
			exec: info.Mode()&0o111 != 0,
			size: info.Size(),
		})
		return nil
	})
	if err != nil {
		return "", 0, err
	}

	sort.Slice(items, func(i, j int) bool {
		return comparePathParts(items[i].rel, items[j].rel) < 0
	})

	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return "", 0, err
	}
	tmpPath := archivePath + ".part"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}

	hasher := sha256.New()
	var nBytes countWriter
	mw := io.MultiWriter(f, hasher, &nBytes)

	gz, err := gzip.NewWriterLevel(mw, gzip.DefaultCompression)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", 0, err
	}
	gz.Header = gzip.Header{OS: 255}
	tw := tar.NewWriter(gz)

	epoch := time.Unix(0, 0).UTC()
	formatFor := func(name string) tar.Format {
		if len(name) > 99 {
			return tar.FormatPAX
		}
		return tar.FormatUSTAR
	}
	for _, it := range items {
		if it.isDir {
			name := it.rel + "/"
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     name,
				Mode:     0o755,
				ModTime:  epoch,
				Format:   formatFor(name),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				_ = f.Close()
				_ = os.Remove(tmpPath)
				return "", 0, err
			}
			continue
		}
		mode := int64(0o644)
		if it.exec {
			mode = 0o755
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     it.rel,
			Size:     it.size,
			Mode:     mode,
			ModTime:  epoch,
			Format:   formatFor(it.rel),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
			return "", 0, err
		}
		src, err := os.Open(it.full)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
			return "", 0, err
		}
		_, copyErr := io.Copy(tw, src)
		_ = src.Close()
		if copyErr != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
			return "", 0, copyErr
		}
	}

	if err := tw.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", 0, err
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, err
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nBytes.n, nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// unwrapKernel extracts a bare arm64 Linux Image from an Alpine EFI zboot
// PE binary ("MZ" + "zimg" + gzip payload) or raw gzipped Image.
func unwrapKernel(data []byte) ([]byte, error) {
	switch {
	case len(data) >= 32 && data[0] == 'M' && data[1] == 'Z' && string(data[4:8]) == "zimg":
		offset := binary.LittleEndian.Uint32(data[8:12])
		size := binary.LittleEndian.Uint32(data[12:16])
		kind := strings.TrimRight(string(data[24:32]), "\x00")
		if kind != "gzip" {
			return nil, fmt.Errorf("kernel: zboot payload is %q, only gzip is supported", kind)
		}
		if uint64(offset)+uint64(size) > uint64(len(data)) {
			return nil, fmt.Errorf("kernel: zboot payload [%d:%d] exceeds %d bytes", offset, uint64(offset)+uint64(size), len(data))
		}
		plain, err := gunzipBytes(data[offset : offset+size])
		if err != nil {
			return nil, fmt.Errorf("kernel: gunzip zboot payload: %w", err)
		}
		data = plain
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		plain, err := gunzipBytes(data)
		if err != nil {
			return nil, fmt.Errorf("kernel: gunzip image: %w", err)
		}
		data = plain
	}

	if len(data) < 60 || string(data[56:60]) != "ARM\x64" {
		return nil, fmt.Errorf("kernel: unwrapped image lacks arm64 magic")
	}
	return data, nil
}

func gunzipBytes(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// resolveModules returns the kernel module paths (as listed in modules.dep)
// required for want, ordered so every dependency appears before modules
// that depend on it.
func resolveModules(modulesDep string, want []string) ([]string, error) {
	deps := make(map[string][]string)
	byName := make(map[string]string)

	for _, line := range strings.Split(modulesDep, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		modPath, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		modPath = strings.TrimSpace(modPath)
		deps[modPath] = strings.Fields(rest)
		base := path.Base(modPath)
		name := strings.Split(base, ".ko")[0]
		name = strings.ReplaceAll(name, "-", "_")
		byName[name] = modPath
	}

	var order []string
	seen := make(map[string]bool)
	visiting := make(map[string]bool)

	var visit func(string) error
	visit = func(p string) error {
		if seen[p] {
			return nil
		}
		if visiting[p] {
			return fmt.Errorf("module dependency cycle at %s", p)
		}
		visiting[p] = true
		for _, d := range deps[p] {
			if err := visit(d); err != nil {
				return err
			}
		}
		visiting[p] = false
		seen[p] = true
		order = append(order, p)
		return nil
	}

	for _, name := range want {
		norm := strings.ReplaceAll(name, "-", "_")
		p, ok := byName[norm]
		if !ok {
			return nil, fmt.Errorf("module %q not found in modules.dep", name)
		}
		if err := visit(p); err != nil {
			return nil, err
		}
	}
	return order, nil
}
