package localreg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type uploadSession struct {
	mu     sync.Mutex
	id     string
	repo   string
	path   string
	file   *os.File
	hasher hash.Hash
	size   int64
	closed bool
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(apiVersionHeader, apiVersionValue)

	if s.dir == "" {
		writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", "registry storage directory not configured")
		return
	}

	route, status, code, msg := parseV2Path(r.URL.RawPath, r.URL.Path)
	if status != 0 {
		writeOCIError(w, r, status, code, msg)
		return
	}

	switch route.kind {
	case routeBase:
		s.handleBase(w, r)
	case routeCatalog:
		s.handleCatalog(w, r)
	case routeTagsList:
		s.handleTagsList(w, r, route)
	case routeBlob:
		s.handleBlob(w, r, route)
	case routeUploadsInit:
		s.handleUploadsInit(w, r, route)
	case routeUpload:
		s.handleUpload(w, r, route)
	case routeManifest:
		s.handleManifest(w, r, route)
	default:
		writeOCIError(w, r, http.StatusNotFound, "UNSUPPORTED", "not found")
	}
}

func writeOCIError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set(apiVersionHeader, apiVersionValue)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	body, _ := json.Marshal(ociErrorResponse{
		Errors: []ociError{{Code: code, Message: message}},
	})
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func writeJSONResponse(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (s *Server) handleBase(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOCIError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	body := []byte("{}")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func paginateList(items []string, r *http.Request, basePath string) ([]string, string, error) {
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		idx := sort.SearchStrings(items, last)
		if idx < len(items) && items[idx] == last {
			idx++
		}
		items = items[idx:]
	}
	nStr := q.Get("n")
	if nStr == "" {
		if items == nil {
			items = []string{}
		}
		return items, "", nil
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 0 {
		return nil, "", fmt.Errorf("invalid n query parameter")
	}
	if n == 0 {
		return []string{}, "", nil
	}
	if len(items) > n {
		items = items[:n]
		lastItem := items[len(items)-1]
		link := fmt.Sprintf("<%s?last=%s&n=%d>; rel=\"next\"", basePath, url.QueryEscape(lastItem), n)
		return items, link, nil
	}
	if items == nil {
		items = []string{}
	}
	return items, "", nil
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOCIError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}

	s.mu.RLock()
	repos, err := s.listRepositoriesLocked()
	s.mu.RUnlock()
	if err != nil {
		writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
		return
	}

	page, link, err := paginateList(repos, r, "/v2/_catalog")
	if err != nil {
		writeOCIError(w, r, http.StatusBadRequest, "UNSUPPORTED", err.Error())
		return
	}
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSONResponse(w, r, http.StatusOK, struct {
		Repositories []string `json:"repositories"`
	}{Repositories: page})
}

func (s *Server) handleTagsList(w http.ResponseWriter, r *http.Request, route v2Route) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOCIError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}

	repoHash := sha256HexString(route.repo)
	s.mu.RLock()
	tagRecords, err := s.readRepoTagsLocked(repoHash)
	s.mu.RUnlock()
	if err != nil {
		writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
		return
	}
	if len(tagRecords) == 0 {
		writeOCIError(w, r, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
		return
	}

	tags := make([]string, 0, len(tagRecords))
	for _, tr := range tagRecords {
		tags = append(tags, tr.Tag)
	}
	sort.Strings(tags)

	page, link, err := paginateList(tags, r, "/v2/"+route.repo+"/tags/list")
	if err != nil {
		writeOCIError(w, r, http.StatusBadRequest, "UNSUPPORTED", err.Error())
		return
	}
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSONResponse(w, r, http.StatusOK, struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{Name: route.repo, Tags: page})
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request, route v2Route) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.mu.RLock()
		f, err := os.Open(s.blobPath(route.digestHex))
		s.mu.RUnlock()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				writeOCIError(w, r, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
				return
			}
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}
		w.Header().Set("Docker-Content-Digest", route.digest)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"`+route.digest+`"`)
		http.ServeContent(w, r, route.digestHex, info.ModTime(), f)

	case http.MethodDelete:
		s.mu.Lock()
		err := os.Remove(s.blobPath(route.digestHex))
		s.mu.Unlock()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				writeOCIError(w, r, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
				return
			}
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}
		s.notifyChange()
		w.WriteHeader(http.StatusAccepted)

	default:
		writeOCIError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func formatUploadRange(size int64) string {
	if size <= 0 {
		return "0-0"
	}
	return fmt.Sprintf("0-%d", size-1)
}

func parseContentRange(hdr string) (start, end int64, ok bool) {
	s := strings.TrimSpace(hdr)
	s = strings.TrimPrefix(s, "bytes ")
	if before, _, cut := strings.Cut(s, "/"); cut {
		s = before
	}
	a, b, cut := strings.Cut(s, "-")
	if !cut {
		return 0, 0, false
	}
	var err error
	start, err = strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	end, err = strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, end, true
}

func (s *Server) handleUploadsInit(w http.ResponseWriter, r *http.Request, route v2Route) {
	if r.Method != http.MethodPost {
		writeOCIError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}

	q := r.URL.Query()

	// 1. Cross-repo blob mount (?mount=<digest>&from=<other>).
	if mount := q.Get("mount"); mount != "" {
		if mountHex, ok := parseDigest(mount); ok {
			s.mu.RLock()
			exists := s.blobExistsLocked(mountHex)
			s.mu.RUnlock()
			if exists {
				w.Header().Set("Location", "/v2/"+route.repo+"/blobs/"+mount)
				w.Header().Set("Docker-Content-Digest", mount)
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusCreated)
				return
			}
		}
		// Per OCI spec, fall back to starting a normal upload when mount cannot be satisfied.
		s.startChunkedUpload(w, r, route)
		return
	}

	// 2. Monolithic single-POST upload (?digest=<digest>).
	if digest := q.Get("digest"); digest != "" {
		expectedHex, ok := parseDigest(digest)
		if !ok {
			writeOCIError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")
			return
		}
		if err := os.MkdirAll(s.uploadsDir(), 0o700); err != nil {
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}
		tmp, err := os.CreateTemp(s.uploadsDir(), "mono-*")
		if err != nil {
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}
		tmpPath := tmp.Name()
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(tmp, h), r.Body); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			writeOCIError(w, r, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			writeOCIError(w, r, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}

		actualHex := hex.EncodeToString(h.Sum(nil))
		if actualHex != expectedHex {
			_ = os.Remove(tmpPath)
			writeOCIError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "provided digest did not match uploaded content")
			return
		}

		s.mu.Lock()
		err = s.commitBlobLocked(tmpPath, expectedHex)
		s.mu.Unlock()
		if err != nil {
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}

		w.Header().Set("Location", "/v2/"+route.repo+"/blobs/"+digest)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
		return
	}

	// 3. Initiate chunked/streamed upload.
	s.startChunkedUpload(w, r, route)
}

func (s *Server) startChunkedUpload(w http.ResponseWriter, r *http.Request, route v2Route) {
	if err := os.MkdirAll(s.uploadsDir(), 0o700); err != nil {
		writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
		return
	}
	id, err := newUUID()
	if err != nil {
		writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
		return
	}
	path := s.uploadPath(id)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
		return
	}

	sess := &uploadSession{
		id:     id,
		repo:   route.repo,
		path:   path,
		file:   f,
		hasher: sha256.New(),
	}
	s.uploadMu.Lock()
	s.uploads[id] = sess
	s.uploadMu.Unlock()

	w.Header().Set("Location", "/v2/"+route.repo+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Range", "0-0")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) getUploadSession(id, repo string) *uploadSession {
	s.uploadMu.Lock()
	sess := s.uploads[id]
	s.uploadMu.Unlock()
	if sess == nil || sess.repo != repo {
		return nil
	}
	return sess
}

func (s *Server) removeUploadSession(id string) *uploadSession {
	s.uploadMu.Lock()
	sess := s.uploads[id]
	delete(s.uploads, id)
	s.uploadMu.Unlock()
	return sess
}

func (s *Server) closeAllUploads() {
	s.uploadMu.Lock()
	sessions := make([]*uploadSession, 0, len(s.uploads))
	for id, sess := range s.uploads {
		sessions = append(sessions, sess)
		delete(s.uploads, id)
	}
	s.uploadMu.Unlock()

	for _, sess := range sessions {
		sess.mu.Lock()
		if !sess.closed {
			sess.closed = true
			if sess.file != nil {
				_ = sess.file.Close()
				sess.file = nil
			}
			_ = os.Remove(sess.path)
		}
		sess.mu.Unlock()
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, route v2Route) {
	switch r.Method {
	case http.MethodGet:
		sess := s.getUploadSession(route.uuid, route.repo)
		if sess == nil {
			writeOCIError(w, r, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
			return
		}
		sess.mu.Lock()
		if sess.closed {
			sess.mu.Unlock()
			writeOCIError(w, r, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
			return
		}
		size := sess.size
		sess.mu.Unlock()

		w.Header().Set("Location", "/v2/"+route.repo+"/blobs/uploads/"+route.uuid)
		w.Header().Set("Docker-Upload-UUID", route.uuid)
		w.Header().Set("Range", formatUploadRange(size))
		w.WriteHeader(http.StatusNoContent)

	case http.MethodPatch:
		sess := s.getUploadSession(route.uuid, route.repo)
		if sess == nil {
			writeOCIError(w, r, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
			return
		}

		sess.mu.Lock()
		if sess.closed {
			sess.mu.Unlock()
			writeOCIError(w, r, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
			return
		}

		if cr := r.Header.Get("Content-Range"); cr != "" {
			start, end, ok := parseContentRange(cr)
			expectedLen := end - start + 1
			if !ok || start != sess.size || (r.ContentLength >= 0 && r.ContentLength != expectedLen) {
				curRange := formatUploadRange(sess.size)
				sess.mu.Unlock()
				w.Header().Set("Location", "/v2/"+route.repo+"/blobs/uploads/"+route.uuid)
				w.Header().Set("Docker-Upload-UUID", route.uuid)
				w.Header().Set("Range", curRange)
				writeOCIError(w, r, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "invalid content range")
				return
			}
		}

		n, err := io.Copy(io.MultiWriter(sess.file, sess.hasher), r.Body)
		if err != nil {
			sess.mu.Unlock()
			writeOCIError(w, r, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		sess.size += n
		newRange := formatUploadRange(sess.size)
		sess.mu.Unlock()

		w.Header().Set("Location", "/v2/"+route.repo+"/blobs/uploads/"+route.uuid)
		w.Header().Set("Docker-Upload-UUID", route.uuid)
		w.Header().Set("Range", newRange)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)

	case http.MethodPut:
		digest := r.URL.Query().Get("digest")
		expectedHex, ok := parseDigest(digest)
		if !ok {
			writeOCIError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "missing or invalid digest")
			return
		}

		sess := s.getUploadSession(route.uuid, route.repo)
		if sess == nil {
			writeOCIError(w, r, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
			return
		}

		sess.mu.Lock()
		if sess.closed {
			sess.mu.Unlock()
			writeOCIError(w, r, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
			return
		}

		if cr := r.Header.Get("Content-Range"); cr != "" {
			start, end, ok := parseContentRange(cr)
			expectedLen := end - start + 1
			if !ok || start != sess.size || (r.ContentLength >= 0 && r.ContentLength != expectedLen) {
				curRange := formatUploadRange(sess.size)
				sess.mu.Unlock()
				w.Header().Set("Location", "/v2/"+route.repo+"/blobs/uploads/"+route.uuid)
				w.Header().Set("Docker-Upload-UUID", route.uuid)
				w.Header().Set("Range", curRange)
				writeOCIError(w, r, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "invalid content range")
				return
			}
		}

		if r.Body != nil {
			n, err := io.Copy(io.MultiWriter(sess.file, sess.hasher), r.Body)
			if err != nil {
				sess.closed = true
				_ = sess.file.Close()
				sess.file = nil
				path := sess.path
				sess.mu.Unlock()
				s.removeUploadSession(route.uuid)
				_ = os.Remove(path)
				writeOCIError(w, r, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", err.Error())
				return
			}
			sess.size += n
		}

		actualHex := hex.EncodeToString(sess.hasher.Sum(nil))
		sess.closed = true
		closeErr := sess.file.Close()
		sess.file = nil
		path := sess.path
		sess.mu.Unlock()

		s.removeUploadSession(route.uuid)

		if closeErr != nil {
			_ = os.Remove(path)
			writeOCIError(w, r, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", closeErr.Error())
			return
		}

		if actualHex != expectedHex {
			_ = os.Remove(path)
			writeOCIError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "provided digest did not match uploaded content")
			return
		}

		s.mu.Lock()
		err := s.commitBlobLocked(path, expectedHex)
		s.mu.Unlock()
		if err != nil {
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}

		w.Header().Set("Location", "/v2/"+route.repo+"/blobs/"+digest)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)

	case http.MethodDelete:
		s.uploadMu.Lock()
		sess := s.uploads[route.uuid]
		if sess != nil && sess.repo == route.repo {
			delete(s.uploads, route.uuid)
		} else {
			sess = nil
		}
		s.uploadMu.Unlock()

		if sess == nil {
			writeOCIError(w, r, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
			return
		}

		sess.mu.Lock()
		if !sess.closed {
			sess.closed = true
			if sess.file != nil {
				_ = sess.file.Close()
				sess.file = nil
			}
			_ = os.Remove(sess.path)
		}
		sess.mu.Unlock()

		w.WriteHeader(http.StatusNoContent)

	default:
		writeOCIError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func extractMediaType(headerCT, manifestMT string) string {
	ct := strings.TrimSpace(headerCT)
	if ct != "" {
		if parsed, _, err := mime.ParseMediaType(ct); err == nil && parsed != "" {
			ct = parsed
		}
	}
	if ct == "" || ct == "application/octet-stream" || ct == "text/plain" {
		if mt := strings.TrimSpace(manifestMT); mt != "" {
			return mt
		}
		return mediaTypeOCIImageManifest
	}
	return ct
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request, route v2Route) {
	repoHash := sha256HexString(route.repo)

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.mu.RLock()
		digestHex := route.digestHex
		if !route.isDigest {
			tr, err := s.readTagLocked(repoHash, route.tag)
			if err != nil {
				s.mu.RUnlock()
				if errors.Is(err, fs.ErrNotExist) {
					writeOCIError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
					return
				}
				writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
				return
			}
			var ok bool
			digestHex, ok = parseDigest(tr.Digest)
			if !ok {
				s.mu.RUnlock()
				writeOCIError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
				return
			}
		}

		raw, err := os.ReadFile(s.revisionPath(repoHash, digestHex))
		if err != nil {
			s.mu.RUnlock()
			if errors.Is(err, fs.ErrNotExist) {
				writeOCIError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
				return
			}
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}
		meta, metaErr := s.readManifestMetaLocked(repoHash, digestHex)
		s.mu.RUnlock()

		mediaType := mediaTypeOCIImageManifest
		if metaErr == nil && meta.MediaType != "" {
			mediaType = meta.MediaType
		}

		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Docker-Content-Digest", "sha256:"+digestHex)
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}

	case http.MethodPut:
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxManifestSize+1))
		if err != nil {
			writeOCIError(w, r, http.StatusInternalServerError, "MANIFEST_INVALID", err.Error())
			return
		}
		if len(raw) == 0 {
			writeOCIError(w, r, http.StatusBadRequest, "MANIFEST_INVALID", "empty manifest body")
			return
		}
		if len(raw) > maxManifestSize {
			writeOCIError(w, r, http.StatusRequestEntityTooLarge, "MANIFEST_INVALID", "manifest exceeds maximum size")
			return
		}

		sum := sha256.Sum256(raw)
		digestHex := hex.EncodeToString(sum[:])
		digest := "sha256:" + digestHex

		if route.isDigest && route.digest != digest {
			writeOCIError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "manifest digest does not match reference")
			return
		}

		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			writeOCIError(w, r, http.StatusBadRequest, "MANIFEST_INVALID", "manifest must be a valid JSON object")
			return
		}

		var parsed ociManifest
		if err := json.Unmarshal(raw, &parsed); err != nil {
			writeOCIError(w, r, http.StatusBadRequest, "MANIFEST_INVALID", "invalid manifest JSON")
			return
		}

		mediaType := extractMediaType(r.Header.Get("Content-Type"), parsed.MediaType)

		s.mu.Lock()
		// Verify referenced config & layer blobs exist.
		if parsed.Config != nil {
			cfgHex, ok := parseDigest(parsed.Config.Digest)
			if !ok {
				s.mu.Unlock()
				writeOCIError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "invalid config digest")
				return
			}
			if !s.blobExistsLocked(cfgHex) {
				s.mu.Unlock()
				writeOCIError(w, r, http.StatusBadRequest, "BLOB_UNKNOWN", "config blob unknown to registry")
				return
			}
		}
		for _, layer := range parsed.Layers {
			layerHex, ok := parseDigest(layer.Digest)
			if !ok {
				s.mu.Unlock()
				writeOCIError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "invalid layer digest")
				return
			}
			if !s.blobExistsLocked(layerHex) {
				s.mu.Unlock()
				writeOCIError(w, r, http.StatusBadRequest, "BLOB_UNKNOWN", "layer blob unknown to registry")
				return
			}
		}
		// For OCI index / Docker manifest list, verify referenced child manifests exist in this repo.
		for _, child := range parsed.Manifests {
			childHex, ok := parseDigest(child.Digest)
			if !ok {
				s.mu.Unlock()
				writeOCIError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "invalid child manifest digest")
				return
			}
			if !s.manifestExistsLocked(repoHash, childHex) && !s.blobExistsLocked(childHex) {
				s.mu.Unlock()
				writeOCIError(w, r, http.StatusBadRequest, "BLOB_UNKNOWN", "referenced child manifest unknown to registry")
				return
			}
		}

		err = s.putManifestLocked(route.repo, digestHex, mediaType, raw, route.tag, time.Now())
		s.mu.Unlock()
		if err != nil {
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}

		s.notifyChange()

		w.Header().Set("Location", "/v2/"+route.repo+"/manifests/"+digest)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)

	case http.MethodDelete:
		s.mu.Lock()
		if !route.isDigest {
			err := os.Remove(s.tagPath(repoHash, route.tag))
			s.mu.Unlock()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					writeOCIError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
					return
				}
				writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
				return
			}
			s.notifyChange()
			w.WriteHeader(http.StatusAccepted)
			return
		}

		if err := os.Remove(s.revisionPath(repoHash, route.digestHex)); err != nil {
			s.mu.Unlock()
			if errors.Is(err, fs.ErrNotExist) {
				writeOCIError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
				return
			}
			writeOCIError(w, r, http.StatusInternalServerError, "UNSUPPORTED", err.Error())
			return
		}
		_ = os.Remove(s.revisionMetaPath(repoHash, route.digestHex))

		// Remove any tags in this repository pointing to the deleted digest.
		if tags, err := s.readRepoTagsLocked(repoHash); err == nil {
			for _, tr := range tags {
				if tr.Digest == route.digest {
					_ = os.Remove(s.tagPath(repoHash, tr.Tag))
				}
			}
		}
		s.mu.Unlock()

		s.notifyChange()
		w.WriteHeader(http.StatusAccepted)

	default:
		writeOCIError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}
