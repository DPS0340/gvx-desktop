package imagefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type gvxImageRecord struct {
	Version      int      `json:"version"`
	Repository   string   `json:"repository"`
	Digest       string   `json:"digest"`
	Architecture string   `json:"architecture"`
	Format       string   `json:"format"`
	Manifests    [][]byte `json:"manifests"`
	Config       []byte   `json:"config"`
}

type ociManifest struct {
	Layers []struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"layers"`
}

type ociConfig struct {
	History []struct {
		Created    time.Time `json:"created"`
		CreatedBy  string    `json:"created_by"`
		EmptyLayer bool      `json:"empty_layer"`
	} `json:"history"`
}

// gvxFS overlays the compiled EROFS layer files of one cached gvx image.
type gvxFS struct {
	layers     []Layer
	erofsFiles []*erofsImage // bottom-to-top (index 0 = oldest layer), nil for empty layers
	// byLayerIndex maps 1-based Layer.Index to its *erofsImage (or nil).
	byLayerIndex map[int]*erofsImage
}

// OpenGVX locates a cached image in cacheDir by its manifest digest or config
// ID and opens its compiled EROFS layers for read-only browsing.
func OpenGVX(cacheDir, imageID, imageDigest string) (FS, error) {
	if cacheDir == "" {
		return nil, errors.New("layer cache directory is unknown")
	}
	rec, err := findGVXImageRecord(cacheDir, imageID, imageDigest)
	if err != nil {
		return nil, err
	}
	if len(rec.Manifests) == 0 {
		return nil, errors.New("cached image has no manifest")
	}
	var man ociManifest
	if err := json.Unmarshal(rec.Manifests[len(rec.Manifests)-1], &man); err != nil {
		return nil, fmt.Errorf("decode image manifest: %w", err)
	}
	var cfg ociConfig
	_ = json.Unmarshal(rec.Config, &cfg)

	fs := &gvxFS{byLayerIndex: map[int]*erofsImage{}}
	// Match OCI history entries to filesystem layers in order.
	nonEmptyHist := make([]struct {
		created time.Time
		cmd     string
	}, 0, len(cfg.History))
	for _, h := range cfg.History {
		if !h.EmptyLayer {
			nonEmptyHist = append(nonEmptyHist, struct {
				created time.Time
				cmd     string
			}{h.Created, CleanCommand(h.CreatedBy)})
		}
	}

	idx := 0
	// If history exists, build the layer list in history order (oldest -> newest)
	// attaching EROFS files to non-empty layers.
	layerCursor := 0
	if len(cfg.History) > 0 && len(nonEmptyHist) == len(man.Layers) {
		for _, h := range cfg.History {
			idx++
			l := Layer{
				Index:   idx,
				Created: h.Created,
				Command: CleanCommand(h.CreatedBy),
				Empty:   h.EmptyLayer,
			}
			if !h.EmptyLayer && layerCursor < len(man.Layers) {
				ml := man.Layers[layerCursor]
				layerCursor++
				erofsImg, diskSize, err := openGVXLayerFile(cacheDir, rec.Format, ml.Digest)
				if err != nil {
					_ = fs.Close()
					return nil, err
				}
				l.Digest = ml.Digest
				l.Size = diskSize
				if l.Size == 0 {
					l.Size = ml.Size
				}
				l.HasFiles = true
				fs.erofsFiles = append(fs.erofsFiles, erofsImg)
				fs.byLayerIndex[idx] = erofsImg
			}
			fs.layers = append(fs.layers, l)
		}
	} else {
		for i, ml := range man.Layers {
			idx++
			erofsImg, diskSize, err := openGVXLayerFile(cacheDir, rec.Format, ml.Digest)
			if err != nil {
				_ = fs.Close()
				return nil, err
			}
			l := Layer{
				Index:    idx,
				Digest:   ml.Digest,
				Size:     diskSize,
				HasFiles: true,
			}
			if l.Size == 0 {
				l.Size = ml.Size
			}
			if i < len(nonEmptyHist) {
				l.Created = nonEmptyHist[i].created
				l.Command = nonEmptyHist[i].cmd
			}
			fs.erofsFiles = append(fs.erofsFiles, erofsImg)
			fs.byLayerIndex[idx] = erofsImg
			fs.layers = append(fs.layers, l)
		}
	}
	return fs, nil
}

func openGVXLayerFile(cacheDir, format, digest string) (*erofsImage, int64, error) {
	hexStr := strings.TrimPrefix(strings.TrimSpace(digest), "sha256:")
	p := filepath.Join(cacheDir, hexStr+"."+format+".erofs")
	st, err := os.Stat(p)
	if err != nil {
		return nil, 0, fmt.Errorf("open layer %s: %w", ShortDigest(digest), err)
	}
	img, err := openEROFS(p)
	if err != nil {
		return nil, 0, fmt.Errorf("read layer %s: %w", ShortDigest(digest), err)
	}
	return img, st.Size(), nil
}

func findGVXImageRecord(cacheDir, imageID, imageDigest string) (gvxImageRecord, error) {
	matches, err := filepath.Glob(filepath.Join(cacheDir, "image-*.json"))
	if err != nil {
		return gvxImageRecord{}, err
	}
	wantID := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(imageID), "sha256:"))
	wantDigest := strings.TrimSpace(imageDigest)
	for _, p := range matches {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec gvxImageRecord
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		if wantDigest != "" && rec.Digest == wantDigest {
			return rec, nil
		}
		if wantID != "" && len(rec.Config) > 0 {
			sum := sha256.Sum256(rec.Config)
			cfgHex := hex.EncodeToString(sum[:])
			if strings.HasPrefix(cfgHex, wantID) {
				return rec, nil
			}
		}
	}
	return gvxImageRecord{}, errors.New("image metadata was not found in the layer cache")
}

// ReadGVXImageConfig returns the raw OCI image config JSON for imageID from
// rungv's cache without opening any EROFS layer files.
func ReadGVXImageConfig(cacheDir, imageID string) ([]byte, error) {
	if cacheDir == "" {
		return nil, errors.New("layer cache directory is unknown")
	}
	rec, err := findGVXImageRecord(cacheDir, imageID, "")
	if err != nil {
		return nil, err
	}
	return rec.Config, nil
}

func (f *gvxFS) Layers() []Layer {
	out := make([]Layer, len(f.layers))
	copy(out, f.layers)
	return out
}

func (f *gvxFS) Close() error {
	var first error
	for _, img := range f.erofsFiles {
		if img != nil {
			if err := img.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	f.erofsFiles = nil
	return first
}

func (f *gvxFS) entryFromInode(img *erofsImage, dir, name string, ino erofsInode, layerIdx int) Entry {
	e := Entry{
		Name:    name,
		Path:    path.Join(dir, name),
		Kind:    ino.kind(),
		Mode:    uint32(ino.mode),
		Size:    int64(ino.size),
		UID:     ino.uid,
		GID:     ino.gid,
		ModTime: ino.modTime(),
		Layer:   layerIdx,
	}
	if e.Kind == KindSymlink {
		e.Target, _ = img.readLink(ino)
	}
	return e
}

func (f *gvxFS) ReadDir(_ context.Context, dir string, layer int) ([]Entry, error) {
	dir = CleanPath(dir)
	if layer > 0 {
		img := f.byLayerIndex[layer]
		if img == nil {
			return nil, nil
		}
		ino, ok, err := img.lookup(dir)
		if err != nil {
			return nil, err
		}
		if !ok || ino.kind() != KindDir {
			return nil, nil
		}
		dirents, err := img.readDir(ino)
		if err != nil {
			return nil, err
		}
		var out []Entry
		for _, d := range dirents {
			child, err := img.inode(d.nid)
			if err != nil || child.isWhiteout() {
				continue
			}
			out = append(out, f.entryFromInode(img, dir, d.name, child, layer))
		}
		SortEntries(out)
		return out, nil
	}

	// Overlay from top (newest) layer down to bottom (oldest) layer.
	seen := map[string]bool{}
	whiteouts := map[string]bool{}
	var out []Entry

	for _, l := range slices.Backward(f.layers) {

		img := f.byLayerIndex[l.Index]
		if img == nil {
			continue
		}
		// Check whether an ancestor of dir is opaque or whiteout in this layer.
		blocked, err := ancestorBlocksLower(img, dir)
		if err != nil {
			return nil, err
		}
		ino, ok, err := img.lookup(dir)
		if err != nil {
			return nil, err
		}
		if ok {
			if ino.isWhiteout() || ino.kind() != KindDir {
				break
			}
			dirents, err := img.readDir(ino)
			if err != nil {
				return nil, err
			}
			for _, d := range dirents {
				if seen[d.name] || whiteouts[d.name] {
					continue
				}
				child, err := img.inode(d.nid)
				if err != nil {
					continue
				}
				if child.isWhiteout() {
					whiteouts[d.name] = true
					continue
				}
				seen[d.name] = true
				out = append(out, f.entryFromInode(img, dir, d.name, child, l.Index))
			}
			if img.isOpaque(ino) {
				break
			}
		}
		if blocked {
			break
		}
	}
	SortEntries(out)
	return out, nil
}

// ancestorBlocksLower reports whether any ancestor of dir in img is opaque, a
// whiteout, or a non-directory, meaning lower layers do not contribute under it.
func ancestorBlocksLower(img *erofsImage, dir string) (bool, error) {
	trimmed := strings.Trim(dir, "/")
	if trimmed == "" {
		return false, nil
	}
	cur, err := img.inode(img.rootNid)
	if err != nil {
		return false, err
	}
	parts := strings.Split(trimmed, "/")
	for _, seg := range parts[:len(parts)-1] {
		if cur.kind() != KindDir {
			return true, nil
		}
		if img.isOpaque(cur) {
			return true, nil
		}
		next, ok, err := img.lookupChild(cur, seg)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if next.isWhiteout() || next.kind() != KindDir || img.isOpaque(next) {
			return true, nil
		}
		cur = next
	}
	return false, nil
}

func (f *gvxFS) ReadFile(_ context.Context, filePath string, layer int, limit int64) (FileContent, error) {
	filePath = CleanPath(filePath)
	readFrom := func(img *erofsImage) (FileContent, bool, error) {
		ino, ok, err := img.lookup(filePath)
		if err != nil || !ok {
			return FileContent{}, false, err
		}
		if ino.isWhiteout() {
			return FileContent{}, true, os.ErrNotExist
		}
		if ino.kind() != KindFile {
			return FileContent{}, true, fmt.Errorf("%s is not a regular file", filePath)
		}
		data, err := img.readData(ino, limit)
		if err != nil {
			return FileContent{}, true, err
		}
		return FileContent{
			Data:      data,
			TotalSize: int64(ino.size),
			Truncated: limit > 0 && int64(ino.size) > limit,
			Binary:    IsBinary(data),
		}, true, nil
	}

	if layer > 0 {
		img := f.byLayerIndex[layer]
		if img == nil {
			return FileContent{}, os.ErrNotExist
		}
		fc, ok, err := readFrom(img)
		if err != nil {
			return FileContent{}, err
		}
		if !ok {
			return FileContent{}, os.ErrNotExist
		}
		return fc, nil
	}

	for _, v := range slices.Backward(f.layers) {
		img := f.byLayerIndex[v.Index]
		if img == nil {
			continue
		}
		fc, found, err := readFrom(img)
		if found {
			return fc, err
		}
		if blocked, _ := ancestorBlocksLower(img, filePath); blocked {
			break
		}
	}
	return FileContent{}, os.ErrNotExist
}
