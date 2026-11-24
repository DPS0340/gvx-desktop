package engine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/registry"
)

// ---- image history (GET /images/{name...}/history) ----

type imageHistoryItem struct {
	ID        string   `json:"Id"`
	Created   int64    `json:"Created"`
	CreatedBy string   `json:"CreatedBy"`
	Tags      []string `json:"Tags"`
	Size      int64    `json:"Size"`
	Comment   string   `json:"Comment"`
}

func (e *Engine) imageHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	img, _, err := e.lookupImageForPlatform(r.Context(), name, platform)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var entries []imagefs.GVXImageHistoryEntry
	if info, err := e.runner.Info(r.Context()); err == nil && info.LayerCacheDir != "" {
		if h, err := imagefs.ReadGVXImageHistory(info.LayerCacheDir, img.ID); err == nil && len(h) > 0 {
			entries = h
		}
	}

	var tags []string
	if len(img.RepoTags) > 0 {
		tags = slices.Clone(img.RepoTags)
	}

	if len(entries) > 0 {
		out := make([]imageHistoryItem, 0, len(entries))
		for i := len(entries) - 1; i >= 0; i-- {
			h := entries[i]
			var created int64
			if !h.Created.IsZero() {
				created = h.Created.UTC().Unix()
			}
			item := imageHistoryItem{
				ID:        "<missing>",
				Created:   created,
				CreatedBy: h.CreatedBy,
				Size:      h.Size,
				Comment:   h.Comment,
			}
			if i == len(entries)-1 {
				item.ID = img.ID
				item.Tags = tags
			}
			out = append(out, item)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	// Fallback when the cache record is unavailable (for example, unit tests
	// using a fake runner without on-disk layer metadata).
	var created int64
	if !img.Created.IsZero() {
		created = img.Created.UTC().Unix()
	}
	n := len(img.RootFS.Layers)
	if n == 0 {
		n = 1
	}
	out := make([]imageHistoryItem, 0, n)
	for i := range n {
		item := imageHistoryItem{
			ID:      "<missing>",
			Created: created,
		}
		if i == 0 {
			item.ID = img.ID
			item.Tags = tags
			item.Size = img.Size
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- image import (POST /images/create?fromSrc=...) ----

func (e *Engine) imageImport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fromSrc := strings.TrimSpace(q.Get("fromSrc"))
	repo := strings.TrimSpace(q.Get("repo"))
	tag := strings.TrimSpace(q.Get("tag"))
	message := q.Get("message")
	platform := strings.TrimSpace(q.Get("platform"))

	if err := validateImagePlatform(platform); err != nil {
		writeError(w, r, err)
		return
	}
	arch := hostImageArch
	if platform != "" {
		a := parsePlatformArch(platform)
		if a == "" {
			writeError(w, r, errInvalid("invalid platform: %q", platform))
			return
		}
		arch = a
	}

	target, err := commitTarget(repo, tag)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var cfg ContainerConfig
	if err := applyChanges(&cfg, q["changes"]); err != nil {
		writeError(w, r, err)
		return
	}

	var srcReader io.Reader
	var closeSrc func()
	if fromSrc == "-" {
		srcReader = r.Body
	} else {
		u, err := url.Parse(fromSrc)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeError(w, r, errInvalid("invalid fromSrc %q: only \"-\" or http(s) URLs are supported", fromSrc))
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, fromSrc, nil)
		if err != nil {
			writeError(w, r, errInvalid("invalid fromSrc URL: %v", err))
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeError(w, r, fmt.Errorf("download %s: %w", fromSrc, err))
			return
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			writeError(w, r, errInvalid("download %s: HTTP %s", fromSrc, resp.Status))
			return
		}
		srcReader = resp.Body
		closeSrc = func() { _ = resp.Body.Close() }
	}
	if closeSrc != nil {
		defer closeSrc()
	}

	limit := maxImageLoadBytes
	if limit <= 0 {
		limit = 4 << 30
	}

	br := bufio.NewReader(io.LimitReader(srcReader, limit+1))
	peek, peekErr := br.Peek(6)
	if peekErr != nil && !errors.Is(peekErr, io.EOF) && !errors.Is(peekErr, bufio.ErrBufferFull) {
		writeError(w, r, errInvalid("read import archive: %v", peekErr))
		return
	}
	if len(peek) == 0 {
		writeError(w, r, errInvalid("empty import archive"))
		return
	}

	var tarStream io.Reader = br
	switch {
	case len(peek) >= 2 && peek[0] == 0x1f && peek[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			writeError(w, r, errInvalid("invalid gzip archive: %v", err))
			return
		}
		defer gz.Close()
		tarStream = gz
	case len(peek) >= 3 && peek[0] == 'B' && peek[1] == 'Z' && peek[2] == 'h':
		tarStream = bzip2.NewReader(br)
	case len(peek) >= 6 && bytes.Equal(peek[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		writeError(w, r, errInvalid("xz-compressed archives are not supported"))
		return
	}

	layer, err := e.tempFile("import-")
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer func() { _ = layer.Close(); _ = os.Remove(layer.Name()) }()

	sum := sha256.New()
	size, err := io.Copy(io.MultiWriter(layer, sum), io.LimitReader(tarStream, limit+1))
	if err != nil {
		writeError(w, r, errInvalid("read import archive: %v", err))
		return
	}
	if size == 0 {
		writeError(w, r, errInvalid("empty import archive"))
		return
	}
	if size > limit {
		writeError(w, r, errInvalid("import archive exceeds size limit (%d bytes)", limit))
		return
	}
	if err := validateTarStream(layer); err != nil {
		writeError(w, r, errInvalid("invalid tar archive: %v", err))
		return
	}

	diffID := "sha256:" + hex.EncodeToString(sum.Sum(nil))
	now := time.Now().UTC()
	comment := message
	if comment == "" {
		comment = "Imported from " + fromSrc
	}
	configBytes, err := json.Marshal(map[string]any{
		"created":      now.Format(time.RFC3339Nano),
		"architecture": arch,
		"os":           "linux",
		"config": commitRunConfig{
			User:         cfg.User,
			ExposedPorts: cfg.ExposedPorts,
			Env:          cfg.Env,
			Entrypoint:   cfg.Entrypoint,
			Cmd:          cfg.Cmd,
			Healthcheck:  cfg.Healthcheck,
			Volumes:      cfg.Volumes,
			WorkingDir:   cfg.WorkingDir,
			Labels:       cfg.Labels,
			StopSignal:   cfg.StopSignal,
		},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{diffID},
		},
		"history": []map[string]string{{
			"created":    now.Format(time.RFC3339Nano),
			"created_by": "",
			"comment":    comment,
		}},
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	archive := tarPipe(func(tw *tar.Writer) error {
		return commitArchive(tw, configBytes, nil, layer, size, diffID, target)
	})
	err = e.loadImagesInArch(r.Context(), arch, archive, func(string) {})
	archive.Close()
	if err != nil {
		writeError(w, r, err)
		return
	}

	cfgSum := sha256.Sum256(configBytes)
	id := "sha256:" + hex.EncodeToString(cfgSum[:])

	attrs := map[string]string{"name": id}
	if target != "" {
		attrs["name"] = target
	}
	e.emit(Event{Type: "image", Action: "import", ID: id, Attributes: attrs})
	e.notify()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	b, _ := json.Marshal(map[string]string{"status": id})
	_, _ = w.Write(append(b, '\n'))
}

func validateTarStream(rs io.ReadSeeker) error {
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tr := tar.NewReader(rs)
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			_, err = rs.Seek(0, io.SeekStart)
			return err
		}
		if err != nil {
			return err
		}
	}
}

// ---- registry search (GET /images/search) ----

type imageSearchItem struct {
	Description string `json:"description"`
	IsOfficial  bool   `json:"is_official"`
	IsAutomated bool   `json:"is_automated"`
	Name        string `json:"name"`
	StarCount   int    `json:"star_count"`
}

func (e *Engine) imageSearch(w http.ResponseWriter, r *http.Request) {
	term := strings.TrimSpace(r.URL.Query().Get("term"))
	if term == "" {
		writeError(w, r, errInvalid("search term is required"))
		return
	}
	limit, err := queryInt(r, "limit", 25)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if limit < 1 || limit > 100 {
		writeError(w, r, errInvalid("Limit %d is outside the range of [1, 100]", limit))
		return
	}
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var (
		hasOfficial  bool
		wantOfficial bool
		hasAuto      bool
		wantAuto     bool
		minStars     int
	)
	if vals := f["is-official"]; len(vals) > 0 {
		b, err := strconv.ParseBool(vals[0])
		if err != nil {
			writeError(w, r, errInvalid("Invalid filter 'is-official=%s'", vals[0]))
			return
		}
		hasOfficial, wantOfficial = true, b
	}
	if vals := f["is-automated"]; len(vals) > 0 {
		b, err := strconv.ParseBool(vals[0])
		if err != nil {
			writeError(w, r, errInvalid("Invalid filter 'is-automated=%s'", vals[0]))
			return
		}
		hasAuto, wantAuto = true, b
	}
	if vals := f["stars"]; len(vals) > 0 {
		n, err := strconv.Atoi(vals[0])
		if err != nil || n < 0 {
			writeError(w, r, errInvalid("Invalid filter 'stars=%s'", vals[0]))
			return
		}
		minStars = n
	}

	src := registry.DockerHub
	query := term
	if rest, ok := strings.CutPrefix(term, "quay.io/"); ok && rest != "" {
		src = registry.Quay
		query = rest
	}

	// Ask for extra entries when client-side filters may drop some results.
	fetchLimit := limit
	if hasOfficial || hasAuto || minStars > 0 {
		fetchLimit = min(100, max(limit*3, 50))
	}

	results, err := registry.Search(r.Context(), src, query, fetchLimit)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := make([]imageSearchItem, 0, min(len(results), limit))
	for _, res := range results {
		if hasOfficial && res.Official != wantOfficial {
			continue
		}
		if hasAuto && wantAuto {
			continue
		}
		if res.Stars < minStars {
			continue
		}
		out = append(out, imageSearchItem{
			Description: res.Description,
			IsOfficial:  res.Official,
			IsAutomated: false,
			Name:        res.Ref(),
			StarCount:   res.Stars,
		})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}
