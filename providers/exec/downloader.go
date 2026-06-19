package exec

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
)

// HTTPDownloader is the real Downloader that uses net/http.
type HTTPDownloader struct {
	client *http.Client
}

// NewHTTPDownloader returns a Downloader backed by net/http.
func NewHTTPDownloader() *HTTPDownloader {
	return &HTTPDownloader{client: http.DefaultClient}
}

// Download fetches url and writes the response body to destPath.
// The destination directory is created if it does not exist.
func (d *HTTPDownloader) Download(ctx context.Context, url, destPath string) error {
	logrus.Infof("downloading %s -> %s", url, destPath)

	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("http get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http get %s: status %d", url, resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("write %s: %w", destPath, err)
	}

	return nil
}
