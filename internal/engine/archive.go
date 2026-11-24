package engine

// Owner: W2 (sandbox engine, docs/design/gvx-v020.md §5.3).

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const archivePathStatHeader = "X-Docker-Container-Path-Stat"

type archivePathStat struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	Mtime      time.Time `json:"mtime"`
	LinkTarget string    `json:"linkTarget"`
}

func encodePathStatHeader(st rungv.PathStat) string {
	b, _ := json.Marshal(archivePathStat{
		Name:       st.Name,
		Size:       st.Size,
		Mode:       st.Mode,
		Mtime:      st.Mtime,
		LinkTarget: st.LinkTarget,
	})
	return base64.StdEncoding.EncodeToString(b)
}

func normalizeArchivePath(raw string) string {
	if !strings.HasPrefix(raw, "/") {
		return "/" + raw
	}
	return raw
}

func (e *Engine) archiveLookup(r *http.Request) (Container, string, string, error) {
	if !e.runner.Features().Control {
		return Container{}, "", "", errNotSupported("Copying files to and from a container")
	}
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		return Container{}, "", "", err
	}
	rawPath := r.URL.Query().Get("path")
	if rawPath == "" {
		return Container{}, "", "", errInvalid("path query parameter is required")
	}
	return c, rawPath, normalizeArchivePath(rawPath), nil
}

func (e *Engine) archivePrepareRunning(r *http.Request, stoppedWhat string) (Container, sandbox.Control, string, string, error) {
	c, rawPath, guestPath, err := e.archiveLookup(r)
	if err != nil {
		return Container{}, nil, "", "", err
	}
	if c.State.Status != StatusRunning {
		return Container{}, nil, "", "", errNotSupported(stoppedWhat)
	}
	ctl := e.runner.Control(c.ID)
	if ctl == nil {
		return Container{}, nil, "", "", errConflict("Container %s was started without a control socket; restart it to use cp", c.ID)
	}
	return c, ctl, rawPath, guestPath, nil
}

func mapArchiveError(c Container, rawPath string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sandbox.ErrPaused) || controlCode(err) == rungv.CodeAlreadyPaused {
		return errConflict("Container %s is paused", c.ID)
	}
	switch controlCode(err) {
	case rungv.CodeNotFound:
		return errNotFound("Could not find the file %s in container %s", rawPath, c.Name)
	case rungv.CodeInvalidArgument, rungv.CodeAlreadyExists:
		return errInvalid("%s", execErrorMessage(err))
	}
	msg := strings.ToLower(execErrorMessage(err))
	if strings.Contains(msg, "read-only file system") {
		return &apiError{http.StatusForbidden, "container rootfs is marked read-only"}
	}
	return err
}

func (e *Engine) containerArchiveHead(w http.ResponseWriter, r *http.Request) {
	c, rawPath, guestPath, err := e.archiveLookup(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if c.State.Status != StatusRunning {
		e.containerArchiveStoppedHead(w, r, c, rawPath, guestPath)
		return
	}
	ctl := e.runner.Control(c.ID)
	if ctl == nil {
		writeError(w, r, errConflict("Container %s was started without a control socket; restart it to use cp", c.ID))
		return
	}
	st, err := ctl.StatPath(r.Context(), guestPath)
	if err != nil {
		writeError(w, r, mapArchiveError(c, rawPath, err))
		return
	}
	w.Header().Set(archivePathStatHeader, encodePathStatHeader(st))
	w.WriteHeader(http.StatusOK)
}

func (e *Engine) containerArchiveGet(w http.ResponseWriter, r *http.Request) {
	c, rawPath, guestPath, err := e.archiveLookup(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if c.State.Status != StatusRunning {
		e.containerArchiveStoppedGet(w, r, c, rawPath, guestPath)
		return
	}
	ctl := e.runner.Control(c.ID)
	if ctl == nil {
		writeError(w, r, errConflict("Container %s was started without a control socket; restart it to use cp", c.ID))
		return
	}
	st, err := ctl.StatPath(r.Context(), guestPath)
	if err != nil {
		writeError(w, r, mapArchiveError(c, rawPath, err))
		return
	}
	rc, err := ctl.CopyFrom(r.Context(), guestPath, rungv.CopyOptions{})
	if err != nil {
		writeError(w, r, mapArchiveError(c, rawPath, err))
		return
	}
	defer rc.Close()

	w.Header().Set(archivePathStatHeader, encodePathStatHeader(st))
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (e *Engine) containerArchivePut(w http.ResponseWriter, r *http.Request) {
	c, ctl, rawPath, guestPath, err := e.archivePrepareRunning(r, "Copying files into a container that is not running")
	if err != nil {
		writeError(w, r, err)
		return
	}
	st, err := ctl.StatPath(r.Context(), guestPath)
	if err != nil {
		writeError(w, r, mapArchiveError(c, rawPath, err))
		return
	}
	mode := os.FileMode(st.Mode)
	if !mode.IsDir() && mode&os.ModeSymlink == 0 {
		writeError(w, r, errInvalid("extraction point is not a directory"))
		return
	}

	br := bufio.NewReader(r.Body)
	peek, _ := br.Peek(6)
	var bodyReader io.Reader = br
	var decompressTrack *trackingReader
	switch {
	case len(peek) >= 2 && peek[0] == 0x1f && peek[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			writeError(w, r, errInvalid("invalid gzip archive: %v", err))
			return
		}
		defer gz.Close()
		decompressTrack = &trackingReader{r: gz}
		bodyReader = decompressTrack
	case len(peek) >= 3 && peek[0] == 'B' && peek[1] == 'Z' && peek[2] == 'h':
		decompressTrack = &trackingReader{r: bzip2.NewReader(br)}
		bzBuf := bufio.NewReader(decompressTrack)
		if _, err := bzBuf.Peek(1); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, r, errInvalid("invalid bzip2 archive: %v", err))
			return
		}
		bodyReader = bzBuf
	case len(peek) >= 6 && bytes.Equal(peek[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		writeError(w, r, errInvalid("xz-compressed archives are not supported"))
		return
	}

	opts := rungv.CopyOptions{
		Archive:        queryBool(r, "copyUIDGID"),
		NoOverwriteDir: queryBool(r, "noOverwriteDirNonDir"),
	}
	if err := ctl.CopyTo(r.Context(), guestPath, bodyReader, opts); err != nil {
		if decompressTrack != nil && decompressTrack.err != nil {
			writeError(w, r, errInvalid("invalid compressed archive: %v", decompressTrack.err))
			return
		}
		writeError(w, r, mapArchiveError(c, rawPath, err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

type trackingReader struct {
	r   io.Reader
	err error
}

func (tr *trackingReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && tr.err == nil {
		tr.err = err
	}
	return n, err
}
