package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmassets"
)

var apkbuildSha512Re = regexp.MustCompile(`(?s)sha512sums="([^"]*)"`)

// sha512Entry is one "<sha512>  <filename>" line from an Alpine APKBUILD.
type sha512Entry struct {
	SHA512   string
	Filename string
}

// parseAPKBUILDSha512Sums extracts the ordered sha512sums entries from an
// Alpine APKBUILD file.
func parseAPKBUILDSha512Sums(apkbuild string) ([]sha512Entry, error) {
	m := apkbuildSha512Re.FindStringSubmatch(apkbuild)
	if m == nil {
		return nil, errors.New("APKBUILD has no sha512sums block")
	}
	var out []sha512Entry
	for _, line := range strings.Split(m[1], "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("unexpected sha512sums line %q", line)
		}
		out = append(out, sha512Entry{SHA512: fields[0], Filename: fields[1]})
	}
	if len(out) == 0 {
		return nil, errors.New("APKBUILD sha512sums block is empty")
	}
	return out, nil
}

// downloadPinnedSHA512 downloads url into cachePath (reusing cachePath when
// its SHA-512 already matches wantSHA512).
func downloadPinnedSHA512(ctx context.Context, client *http.Client, url, cachePath, wantSHA512 string) error {
	if got, err := sha512File(cachePath); err == nil {
		if got == wantSHA512 {
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
		err := downloadOnceSHA512(ctx, client, url, cachePath, wantSHA512)
		if err == nil {
			return nil
		}
		lastErr = err
		if errors.Is(err, errNotFound) || errors.Is(err, errHashMismatch) {
			return err
		}
	}
	return lastErr
}

func downloadOnceSHA512(ctx context.Context, client *http.Client, url, cachePath, wantSHA512 string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gvx-mkassets/1")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
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

	h := sha512.New()
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
	if got != wantSHA512 {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%s: got sha512 %s, want %s: %w", url, got, wantSHA512, errHashMismatch)
	}
	return os.Rename(tmpPath, cachePath)
}

func sha512File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha512Bytes(b []byte) string {
	sum := sha512.Sum512(b)
	return hex.EncodeToString(sum[:])
}

// fetchAportsRecipe fetches main/<origin>/ at r.Commit from Alpine's GitLab
// archive API and returns its regular files keyed by path relative to
// main/<origin>/. Every returned APKBUILD is verified against r.APKBUILDSHA256.
func fetchAportsRecipe(ctx context.Context, client *http.Client, cacheDir string, r aportsRecipe) (map[string][]byte, error) {
	tarPath := filepath.Join(cacheDir, "corresponding-sources", "aports-tar", r.Origin+"-"+r.Commit+".tar.gz")
	if _, err := os.Stat(tarPath); err != nil {
		if err := os.MkdirAll(filepath.Dir(tarPath), 0o755); err != nil {
			return nil, err
		}
		var lastErr error
	fetch:
		for _, url := range []string{r.ArchiveURL(), r.MirrorArchiveURL()} {
			for attempt := 1; attempt <= 3; attempt++ {
				if attempt > 1 {
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(time.Duration(attempt) * time.Second):
					}
				}
				err := downloadRaw(ctx, client, url, tarPath)
				if err == nil {
					lastErr = nil
					break fetch
				}
				lastErr = err
			}
			fmt.Printf("mkassets: %s: %v\n", url, lastErr)
		}
		if lastErr != nil {
			return nil, lastErr
		}
	}

	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gunzip aports archive %s: %w", r.Origin, err)
	}
	defer gz.Close()

	prefix := "/main/" + r.Origin + "/"
	files := make(map[string][]byte)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read aports archive %s: %w", r.Origin, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		idx := strings.Index(hdr.Name, prefix)
		if idx < 0 {
			continue
		}
		rel := cleanArchivePath(hdr.Name[idx+len(prefix):])
		if rel == "" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", r.Origin, rel, err)
		}
		files[rel] = data
	}

	apkbuild, ok := files["APKBUILD"]
	if !ok {
		return nil, fmt.Errorf("aports archive %s@%s missing APKBUILD", r.Origin, r.Commit)
	}
	if got := sha256Bytes(apkbuild); got != r.APKBUILDSHA256 {
		_ = os.Remove(tarPath)
		return nil, fmt.Errorf("aports %s APKBUILD sha256 %s, want %s", r.Origin, got, r.APKBUILDSHA256)
	}
	return files, nil
}

func downloadRaw(ctx context.Context, client *http.Client, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gvx-mkassets/1")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("read %s: %w", url, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dst)
}

// buildSourcesArchive assembles a deterministic corresponding-source tarball
// containing:
//   - SOURCES and LICENSES/*
//   - each pinned Alpine APKBUILD and all patches/auxiliary files at the
//     exact pinned aports commit, verified against APKBUILD's SHA-256 and
//     sha512sums
//   - every upstream source tarball listed by those APKBUILDs, verified
//     against its pinned SHA-512
//   - immutable BuildKit and runc source refs (buildkit-runc/REFS.txt)
func buildSourcesArchive(ctx context.Context, client *http.Client, repoDir, cacheDir, sourcesText, outArchivePath string) (string, int64, error) {
	stageDir, err := os.MkdirTemp("", "gvx-mkassets-sources-*")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(stageDir)

	if err := writeFile(filepath.Join(stageDir, vmassets.SourcesFile), []byte(sourcesText), 0o644); err != nil {
		return "", 0, err
	}
	if err := copyLicensesDir(repoDir, filepath.Join(stageDir, "LICENSES")); err != nil {
		return "", 0, err
	}

	srcCache := filepath.Join(cacheDir, "corresponding-sources")

	for _, r := range aportsRecipes {
		fmt.Printf("mkassets: sources %s %s @ %s\n", r.Origin, r.Version, r.Commit[:12])
		recipeFiles, err := fetchAportsRecipe(ctx, client, cacheDir, r)
		if err != nil {
			return "", 0, fmt.Errorf("sources %s: %w", r.Origin, err)
		}

		// Write every file from main/<origin>/ at the pinned commit.
		relNames := make([]string, 0, len(recipeFiles))
		for name := range recipeFiles {
			relNames = append(relNames, name)
		}
		sort.Strings(relNames)
		for _, name := range relNames {
			if err := writeFile(filepath.Join(stageDir, "aports", r.Origin, filepath.FromSlash(name)), recipeFiles[name], 0o644); err != nil {
				return "", 0, err
			}
		}

		entries, err := parseAPKBUILDSha512Sums(string(recipeFiles["APKBUILD"]))
		if err != nil {
			return "", 0, fmt.Errorf("sources %s: %w", r.Origin, err)
		}

		upstreamByName := make(map[string]upstreamSource, len(r.UpstreamSources))
		for _, u := range r.UpstreamSources {
			upstreamByName[u.Filename] = u
		}
		matchedUpstream := make(map[string]bool, len(r.UpstreamSources))

		for _, e := range entries {
			if u, isUpstream := upstreamByName[e.Filename]; isUpstream {
				if e.SHA512 != u.SHA512 {
					return "", 0, fmt.Errorf("sources %s/%s: pinned sha512 differs from APKBUILD", r.Origin, e.Filename)
				}
				matchedUpstream[e.Filename] = true
				upCache := filepath.Join(srcCache, "upstream", r.Origin, e.Filename)
				fmt.Printf("mkassets: fetch upstream %s\n", u.Filename)
				var err error
				for _, url := range append([]string{u.URL}, u.Mirrors...) {
					if err = downloadPinnedSHA512(ctx, client, url, upCache, u.SHA512); err == nil {
						break
					}
					fmt.Printf("mkassets: %s: %v\n", url, err)
				}
				if err != nil {
					return "", 0, fmt.Errorf("sources upstream %s: %w", u.Filename, err)
				}
				if err := copyFile(upCache, filepath.Join(stageDir, "upstream", r.Origin, e.Filename), 0o644); err != nil {
					return "", 0, err
				}
				continue
			}

			// Verify every local patch/file listed in sha512sums against the
			// file extracted from the pinned aports archive.
			localData, ok := recipeFiles[e.Filename]
			if !ok {
				return "", 0, fmt.Errorf("sources aports %s: file %s listed in sha512sums missing from archive", r.Origin, e.Filename)
			}
			if got := sha512Bytes(localData); got != e.SHA512 {
				return "", 0, fmt.Errorf("sources aports %s/%s: sha512 %s, want %s", r.Origin, e.Filename, got, e.SHA512)
			}
		}

		for _, u := range r.UpstreamSources {
			if !matchedUpstream[u.Filename] {
				return "", 0, fmt.Errorf("sources %s: pinned upstream %s not found in APKBUILD sha512sums", r.Origin, u.Filename)
			}
		}
	}

	refs := formatBuildkitRuncRefs()
	if err := writeFile(filepath.Join(stageDir, "buildkit-runc", "REFS.txt"), []byte(refs), 0o644); err != nil {
		return "", 0, err
	}

	sha, size, err := writeDeterministicTarGz(stageDir, outArchivePath)
	if err != nil {
		return "", 0, fmt.Errorf("write sources archive: %w", err)
	}
	shaLine := fmt.Sprintf("%s  %s\n", sha, filepath.Base(outArchivePath))
	if err := writeFile(outArchivePath+".sha256", []byte(shaLine), 0o644); err != nil {
		return "", 0, err
	}
	return sha, size, nil
}

func formatBuildkitRuncRefs() string {
	var sb strings.Builder
	sb.WriteString("BuildKit and runc immutable source references\n")
	sb.WriteString("=============================================\n\n")
	fmt.Fprintf(&sb, "moby/buildkit %s\n", buildkitPin.Tag)
	fmt.Fprintf(&sb, "  license:  %s\n", buildkitPin.License)
	fmt.Fprintf(&sb, "  commit:   %s\n", buildkitPin.Commit)
	fmt.Fprintf(&sb, "  repo:     https://github.com/moby/buildkit\n")
	fmt.Fprintf(&sb, "  tree:     %s\n", buildkitPin.UpstreamURL)
	fmt.Fprintf(&sb, "  tarball:  https://github.com/moby/buildkit/archive/%s.tar.gz\n", buildkitPin.Commit)
	fmt.Fprintf(&sb, "  index:    %s@%s\n", buildkitPin.Repo, buildkitPin.IndexDigest)
	fmt.Fprintf(&sb, "  manifest: %s@%s\n\n", buildkitPin.Repo, buildkitPin.ManifestDigest)

	fmt.Fprintf(&sb, "opencontainers/runc %s (shipped as buildkit-runc)\n", buildkitPin.RuncVersion)
	fmt.Fprintf(&sb, "  license:  %s\n", buildkitPin.RuncLicense)
	fmt.Fprintf(&sb, "  commit:   %s\n", buildkitPin.RuncCommit)
	fmt.Fprintf(&sb, "  repo:     https://github.com/opencontainers/runc\n")
	fmt.Fprintf(&sb, "  tree:     %s\n", buildkitPin.RuncURL)
	fmt.Fprintf(&sb, "  tarball:  https://github.com/opencontainers/runc/archive/%s.tar.gz\n\n", buildkitPin.RuncCommit)

	sb.WriteString("Note: libseccomp (LGPL-2.1-or-later, statically linked into buildkit-runc)\n")
	sb.WriteString("and ca-certificates (MPL-2.0 AND MIT, Mozilla CA bundle) are included as\n")
	sb.WriteString("full upstream source tarballs and pinned Alpine aports recipes under\n")
	sb.WriteString("upstream/ and aports/ in this archive.\n")
	return sb.String()
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
