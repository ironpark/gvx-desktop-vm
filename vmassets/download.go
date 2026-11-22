package vmassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrTooLarge reports a download past the size it may have.
var ErrTooLarge = errors.New("the download is larger than it may be")

// download writes what url serves to w, reading no more than limit bytes
// of it, and checks it against want, its SHA-256 in hex. A download that
// does not match is an error, and w then holds what is not to be used.
// progress, when not nil, hears how much has arrived of how much.
func download(ctx context.Context, url, name string, size int64, want string, w io.Writer, limit int64, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gvx-desktop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	total := resp.ContentLength
	if total <= 0 {
		total = size
	}
	if total > limit {
		return fmt.Errorf("%w: %s is %d MB, more than %d MB", ErrTooLarge, name, total>>20, limit>>20)
	}
	sum := sha256.New()
	body := io.TeeReader(io.LimitReader(resp.Body, limit+1), sum)
	if progress != nil {
		body = &counter{r: body, total: total, report: progress}
	}
	n, err := io.Copy(w, body)
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%w: %s is more than %d MB", ErrTooLarge, name, limit>>20)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != strings.ToLower(want) {
		return fmt.Errorf("%s does not match its checksum: got %s, want %s", name, got, want)
	}
	return nil
}

type counter struct {
	r      io.Reader
	done   int64
	total  int64
	report func(done, total int64)
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
	c.report(c.done, c.total)
	return n, err
}
