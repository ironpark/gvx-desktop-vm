package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ociAccept = "application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json"
)

type apkEntry struct {
	Name       string
	Typeflag   byte
	Mode       uint32
	Linkname   string
	Data       []byte
	Executable bool
}

// downloadPinned fetches url into cachePath (reusing cachePath when its
// SHA-256 already matches wantSHA256). On mismatch the partial file is
// deleted. If isAlpine and the server answers 404, the error notes that
// Alpine may have rotated the version and the pins need bumping.
func downloadPinned(ctx context.Context, client *http.Client, url, cachePath, wantSHA256 string, headers map[string]string, isAlpine bool) error {
	if got, err := sha256File(cachePath); err == nil {
		if got == wantSHA256 {
			return nil
		}
		_ = os.Remove(cachePath)
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		err := downloadOnce(ctx, client, url, cachePath, wantSHA256, headers, isAlpine)
		if err == nil {
			return nil
		}
		lastErr = err
		// Do not retry 404 or hash mismatches.
		if errors.Is(err, errNotFound) || errors.Is(err, errHashMismatch) {
			return err
		}
	}
	return lastErr
}

var (
	errNotFound     = errors.New("not found")
	errHashMismatch = errors.New("sha256 mismatch")
)

func downloadOnce(ctx context.Context, client *http.Client, url, cachePath, wantSHA256 string, headers map[string]string, isAlpine bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gvx-mkassets/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		if isAlpine {
			return fmt.Errorf("GET %s: HTTP 404 (Alpine dropped this package version; bump the pins in guest/mkassets/pins.go): %w", url, errNotFound)
		}
		return fmt.Errorf("GET %s: HTTP 404: %w", url, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	tmpPath := cachePath + ".part"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("read %s: %w", url, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}

	got := hex.EncodeToString(h.Sum(nil))
	if got != wantSHA256 {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%s: got sha256 %s, want %s: %w", url, got, wantSHA256, errHashMismatch)
	}
	return os.Rename(tmpPath, cachePath)
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

func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// extractAPKData reads the data tar segment (the final concatenated gzip
// stream) of an Alpine .apk package and returns its files and symlinks.
func extractAPKData(apkPath string) ([]apkEntry, error) {
	f, err := os.Open(apkPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<20)
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, fmt.Errorf("apk %s: gzip open: %w", filepath.Base(apkPath), err)
	}
	defer gz.Close()

	var streams [][]byte
	for {
		gz.Multistream(false)
		plain, err := io.ReadAll(gz)
		if err != nil {
			return nil, fmt.Errorf("apk %s: read gzip stream %d: %w", filepath.Base(apkPath), len(streams), err)
		}
		streams = append(streams, plain)
		err = gz.Reset(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("apk %s: next gzip stream: %w", filepath.Base(apkPath), err)
		}
	}
	if len(streams) < 2 {
		return nil, fmt.Errorf("apk %s: expected at least 2 gzip streams, got %d", filepath.Base(apkPath), len(streams))
	}

	// The last stream is the package's data tar.
	dataTar := streams[len(streams)-1]
	tr := tar.NewReader(strings.NewReader(string(dataTar)))
	var entries []apkEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("apk %s: read tar: %w", filepath.Base(apkPath), err)
		}
		name := cleanArchivePath(hdr.Name)
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("apk %s: read %s: %w", filepath.Base(apkPath), name, err)
			}
			entries = append(entries, apkEntry{
				Name:       name,
				Typeflag:   hdr.Typeflag,
				Mode:       uint32(hdr.Mode) & 0o7777,
				Data:       data,
				Executable: hdr.Mode&0o111 != 0,
			})
		case tar.TypeSymlink:
			entries = append(entries, apkEntry{
				Name:     name,
				Typeflag: hdr.Typeflag,
				Linkname: hdr.Linkname,
			})
		}
	}
	return entries, nil
}

// fetchBuildkit verifies the pinned moby/buildkit index and arm64 manifest,
// downloads the pinned layers that hold the needed binaries and CA bundle,
// and returns the extracted files keyed by their paths in the image.
func fetchBuildkit(ctx context.Context, client *http.Client, cacheDir string) (map[string][]byte, error) {
	bkCache := filepath.Join(cacheDir, "buildkit-"+buildkitPin.Tag)
	if err := os.MkdirAll(bkCache, 0o755); err != nil {
		return nil, err
	}

	// Fast path: if every layer we need is already cached with the exact
	// pinned digest, we still verify the cached layer digests and avoid
	// unnecessary registry traffic only if manifest was also cached.
	// Always verify index + manifest (cached by digest too) so a fresh cache
	// checks the full digest chain from IndexDigest -> ManifestDigest -> layers.
	indexPath := filepath.Join(bkCache, digestFilename(buildkitPin.IndexDigest, ".json"))
	manifestPath := filepath.Join(bkCache, digestFilename(buildkitPin.ManifestDigest, ".json"))

	needNet := !hasValidCache(indexPath, buildkitPin.IndexDigest) || !hasValidCache(manifestPath, buildkitPin.ManifestDigest)
	if !needNet {
		for _, l := range buildkitPin.Layers {
			if l.Extract && !hasValidCache(filepath.Join(bkCache, digestFilename(l.Digest, ".tar.gz")), l.Digest) {
				needNet = true
				break
			}
		}
	}

	var authHeaders map[string]string
	if needNet {
		token, err := fetchRegistryToken(ctx, client, buildkitPin.AuthURL)
		if err != nil {
			return nil, err
		}
		authHeaders = map[string]string{
			"Authorization": "Bearer " + token,
			"Accept":        ociAccept,
		}
	}

	// 1. Fetch and verify OCI index by pinned digest.
	idxURL := fmt.Sprintf("%s/v2/%s/manifests/%s", buildkitPin.Registry, buildkitPin.Repo, buildkitPin.IndexDigest)
	if err := downloadPinned(ctx, client, idxURL, indexPath, trimSHA256(buildkitPin.IndexDigest), authHeaders, false); err != nil {
		return nil, fmt.Errorf("buildkit index: %w", err)
	}
	idxRaw, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, err
	}
	var idx struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		return nil, fmt.Errorf("parse buildkit index: %w", err)
	}
	foundArm64 := false
	for _, m := range idx.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == "arm64" && m.Digest == buildkitPin.ManifestDigest {
			foundArm64 = true
			break
		}
	}
	if !foundArm64 {
		return nil, fmt.Errorf("buildkit index %s does not list linux/arm64 manifest %s", buildkitPin.IndexDigest, buildkitPin.ManifestDigest)
	}

	// 2. Fetch and verify arm64 manifest by pinned digest.
	manURL := fmt.Sprintf("%s/v2/%s/manifests/%s", buildkitPin.Registry, buildkitPin.Repo, buildkitPin.ManifestDigest)
	if err := downloadPinned(ctx, client, manURL, manifestPath, trimSHA256(buildkitPin.ManifestDigest), authHeaders, false); err != nil {
		return nil, fmt.Errorf("buildkit manifest: %w", err)
	}
	manRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var man struct {
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manRaw, &man); err != nil {
		return nil, fmt.Errorf("parse buildkit manifest: %w", err)
	}
	if len(man.Layers) != len(buildkitPin.Layers) {
		return nil, fmt.Errorf("buildkit manifest has %d layers, pins say %d", len(man.Layers), len(buildkitPin.Layers))
	}
	for i, want := range buildkitPin.Layers {
		if man.Layers[i].Digest != want.Digest || man.Layers[i].Size != want.Size {
			return nil, fmt.Errorf("buildkit layer %d mismatch: got %s (%d), want %s (%d)",
				i, man.Layers[i].Digest, man.Layers[i].Size, want.Digest, want.Size)
		}
	}

	// 3. Download and extract pinned layers in manifest order (later layers win).
	blobHeaders := map[string]string{}
	if tok, ok := authHeaders["Authorization"]; ok {
		blobHeaders["Authorization"] = tok
	}
	found := make(map[string][]byte, len(buildkitWant))
	for _, l := range buildkitPin.Layers {
		if !l.Extract {
			continue
		}
		layerPath := filepath.Join(bkCache, digestFilename(l.Digest, ".tar.gz"))
		blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", buildkitPin.Registry, buildkitPin.Repo, l.Digest)
		if err := downloadPinned(ctx, client, blobURL, layerPath, trimSHA256(l.Digest), blobHeaders, false); err != nil {
			return nil, fmt.Errorf("buildkit layer %s: %w", l.Digest, err)
		}
		if err := extractLayerFiles(layerPath, found); err != nil {
			return nil, fmt.Errorf("extract buildkit layer %s: %w", l.Digest, err)
		}
	}

	for name := range buildkitWant {
		if _, ok := found[name]; !ok {
			return nil, fmt.Errorf("buildkit: %s was not found in pinned layers", name)
		}
	}
	return found, nil
}

func fetchRegistryToken(ctx context.Context, client *http.Client, authURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "gvx-mkassets/1")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry auth: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("registry auth json: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("registry auth: empty token")
	}
	return out.Token, nil
}

func extractLayerFiles(layerPath string, out map[string][]byte) error {
	f, err := os.Open(layerPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := cleanArchivePath(hdr.Name)
		if _, want := buildkitWant[name]; want && hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			out[name] = b
		}
	}
}

func digestFilename(digest, ext string) string {
	return strings.ReplaceAll(digest, ":", "-") + ext
}

func trimSHA256(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}

func hasValidCache(p, digest string) bool {
	got, err := sha256File(p)
	return err == nil && got == trimSHA256(digest)
}
