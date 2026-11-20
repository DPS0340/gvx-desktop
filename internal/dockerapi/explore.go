package dockerapi

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HistoryEntry is one layer entry of GET /images/{name}/history.
type HistoryEntry struct {
	ID        string   `json:"Id"`
	Created   int64    `json:"Created"`
	CreatedBy string   `json:"CreatedBy"`
	Size      int64    `json:"Size"`
	Tags      []string `json:"Tags"`
	Comment   string   `json:"Comment"`
}

// ImageHistory lists the layers of an image, newest first, as docker history does.
func (c *Client) ImageHistory(ctx context.Context, name string) ([]HistoryEntry, error) {
	var out []HistoryEntry
	err := c.do(ctx, http.MethodGet, "/images/"+escapeImagePath(name)+"/history", &out)
	return out, err
}

// createTempContainer creates a stopped container for image so its merged
// filesystem can be inspected through /containers/{id}/archive without ever
// running code inside the image.
func (c *Client) createTempContainer(ctx context.Context, image string) (string, error) {
	req := struct {
		Image      string   `json:"Image"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
	}{
		Image:      image,
		Entrypoint: []string{"/"},
		Cmd:        []string{"/"},
	}
	var resp struct {
		ID string `json:"Id"`
	}
	if err := c.postJSON(ctx, "/containers/create", req, &resp); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", errors.New("dockerd returned no container ID")
	}
	return resp.ID, nil
}

func (c *Client) removeTempContainer(ctx context.Context, id string) {
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = c.Remove(cleanCtx, id, true)
}

func (c *Client) containerArchive(ctx context.Context, id, filePath string) (io.ReadCloser, error) {
	q := url.Values{"path": {filePath}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/containers/"+url.PathEscape(id)+"/archive?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	// Streaming an archive of a large image can exceed the short API timeout;
	// ctx controls cancellation.
	streamClient := &http.Client{Transport: c.http.Transport}
	res, err := streamClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 300 {
		return res.Body, nil
	}
	defer res.Body.Close()
	var e apiError
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return nil, fmt.Errorf("%s", e.Message)
	}
	return nil, fmt.Errorf("GET /containers/%s/archive: %s", id, res.Status)
}

// ExportImageRootFS streams the merged filesystem of image as a tar archive to
// fn using a temporary non-started container that is removed before returning.
func (c *Client) ExportImageRootFS(ctx context.Context, image string, fn func(tr *tar.Reader) error) error {
	id, err := c.createTempContainer(ctx, image)
	if err != nil {
		return err
	}
	defer c.removeTempContainer(ctx, id)
	rc, err := c.containerArchive(ctx, id, "/")
	if err != nil {
		return err
	}
	defer rc.Close()
	return fn(tar.NewReader(rc))
}

// ReadImageFile reads up to limit bytes of filePath from image's merged
// filesystem using a temporary non-started container.
func (c *Client) ReadImageFile(ctx context.Context, image, filePath string, limit int64) ([]byte, int64, error) {
	id, err := c.createTempContainer(ctx, image)
	if err != nil {
		return nil, 0, err
	}
	defer c.removeTempContainer(ctx, id)
	rc, err := c.containerArchive(ctx, id, filePath)
	if err != nil {
		return nil, 0, err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, 0, fmt.Errorf("file not found in image: %s", filePath)
		}
		if err != nil {
			return nil, 0, err
		}
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			data, err := io.ReadAll(io.LimitReader(tr, limit))
			return data, hdr.Size, err
		}
		if hdr.Typeflag == tar.TypeSymlink {
			return nil, 0, fmt.Errorf("symlink -> %s", hdr.Linkname)
		}
		if hdr.Typeflag == tar.TypeDir {
			return nil, 0, fmt.Errorf("%s is a directory", filePath)
		}
	}
}
