package localreg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Server) blobsDir() string {
	return filepath.Join(s.dir, "blobs", "sha256")
}

func (s *Server) blobPath(hexDigest string) string {
	return filepath.Join(s.dir, "blobs", "sha256", hexDigest)
}

func (s *Server) uploadsDir() string {
	return filepath.Join(s.dir, "uploads")
}

func (s *Server) uploadPath(uuid string) string {
	return filepath.Join(s.dir, "uploads", uuid)
}

func (s *Server) reposDir() string {
	return filepath.Join(s.dir, "repos")
}

func (s *Server) repoDir(repoHash string) string {
	return filepath.Join(s.dir, "repos", repoHash)
}

func (s *Server) repoMetaPath(repoHash string) string {
	return filepath.Join(s.dir, "repos", repoHash, "repo.json")
}

func (s *Server) revisionsDir(repoHash string) string {
	return filepath.Join(s.dir, "repos", repoHash, "revisions", "sha256")
}

func (s *Server) revisionPath(repoHash, hexDigest string) string {
	return filepath.Join(s.dir, "repos", repoHash, "revisions", "sha256", hexDigest)
}

func (s *Server) revisionMetaPath(repoHash, hexDigest string) string {
	return filepath.Join(s.dir, "repos", repoHash, "revisions", "sha256", hexDigest+".meta")
}

func (s *Server) tagsDir(repoHash string) string {
	return filepath.Join(s.dir, "repos", repoHash, "tags")
}

func (s *Server) tagPath(repoHash, tag string) string {
	return filepath.Join(s.dir, "repos", repoHash, "tags", sha256HexString(tag)+".json")
}

func isValidHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// writeFileAtomic writes data to a temporary file in the destination directory,
// closes it (required on Windows before rename), and atomically renames it into place.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func (s *Server) blobExistsLocked(hexDigest string) bool {
	info, err := os.Stat(s.blobPath(hexDigest))
	return err == nil && !info.IsDir()
}

func (s *Server) manifestExistsLocked(repoHash, hexDigest string) bool {
	info, err := os.Stat(s.revisionPath(repoHash, hexDigest))
	return err == nil && !info.IsDir()
}

// commitBlobLocked moves an already-closed staging file into blobs/sha256/<hex>.
// If the immutable content-addressed blob already exists, the staging file is simply removed.
func (s *Server) commitBlobLocked(tmpPath, hexDigest string) error {
	if err := os.MkdirAll(s.blobsDir(), 0o700); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	dst := s.blobPath(hexDigest)
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(tmpPath)
		return nil
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// sweepStaleFiles removes leftover files from uploads/ (excluding any currently
// active in-memory sessions) and orphaned .tmp-* files from previous crashes.
func (s *Server) sweepStaleFiles() error {
	s.uploadMu.Lock()
	active := make(map[string]bool, len(s.uploads))
	for id := range s.uploads {
		active[id] = true
	}
	s.uploadMu.Unlock()

	if entries, err := os.ReadDir(s.uploadsDir()); err == nil {
		for _, e := range entries {
			if active[e.Name()] {
				continue
			}
			_ = os.RemoveAll(filepath.Join(s.uploadsDir(), e.Name()))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// Best-effort sweep of orphaned atomic temp files in blobs/ and repos/.
	sweepTmpInDir(s.blobsDir())
	if repoEntries, err := os.ReadDir(s.reposDir()); err == nil {
		for _, re := range repoEntries {
			if !re.IsDir() || !isValidHex64(re.Name()) {
				continue
			}
			sweepTmpInDir(s.repoDir(re.Name()))
			sweepTmpInDir(s.revisionsDir(re.Name()))
			sweepTmpInDir(s.tagsDir(re.Name()))
		}
	}
	return nil
}

func sweepTmpInDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func (s *Server) readRepoNameLocked(repoHash string) (string, error) {
	b, err := os.ReadFile(s.repoMetaPath(repoHash))
	if err != nil {
		return "", err
	}
	var rm repoMeta
	if err := json.Unmarshal(b, &rm); err != nil {
		return "", err
	}
	return rm.Name, nil
}

func (s *Server) readRepoTagsLocked(repoHash string) ([]tagRecord, error) {
	entries, err := os.ReadDir(s.tagsDir(repoHash))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tags []tagRecord
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.tagsDir(repoHash), e.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var tr tagRecord
		if err := json.Unmarshal(b, &tr); err != nil {
			continue
		}
		if tr.Tag == "" || tr.Digest == "" {
			continue
		}
		tags = append(tags, tr)
	}
	return tags, nil
}

func (s *Server) readManifestMetaLocked(repoHash, hexDigest string) (manifestMeta, error) {
	b, err := os.ReadFile(s.revisionMetaPath(repoHash, hexDigest))
	if err != nil {
		return manifestMeta{}, err
	}
	var mm manifestMeta
	if err := json.Unmarshal(b, &mm); err != nil {
		return manifestMeta{}, err
	}
	return mm, nil
}

func (s *Server) readTagLocked(repoHash, tag string) (tagRecord, error) {
	b, err := os.ReadFile(s.tagPath(repoHash, tag))
	if err != nil {
		return tagRecord{}, err
	}
	var tr tagRecord
	if err := json.Unmarshal(b, &tr); err != nil {
		return tagRecord{}, err
	}
	return tr, nil
}

func (s *Server) putManifestLocked(repo, digestHex, mediaType string, raw []byte, tag string, now time.Time) error {
	repoHash := sha256HexString(repo)
	if err := os.MkdirAll(s.repoDir(repoHash), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(s.repoMetaPath(repoHash)); errors.Is(err, fs.ErrNotExist) {
		if err := writeJSONAtomic(s.repoMetaPath(repoHash), repoMeta{Name: repo}); err != nil {
			return err
		}
	}

	revPath := s.revisionPath(repoHash, digestHex)
	if err := writeFileAtomic(revPath, raw); err != nil {
		return err
	}

	digest := "sha256:" + digestHex
	meta := manifestMeta{
		MediaType: mediaType,
		Digest:    digest,
		Size:      int64(len(raw)),
		UpdatedAt: now.UTC(),
	}
	if err := writeJSONAtomic(s.revisionMetaPath(repoHash, digestHex), meta); err != nil {
		return err
	}

	if tag != "" {
		tr := tagRecord{
			Tag:       tag,
			Digest:    digest,
			UpdatedAt: now.UTC(),
		}
		if err := writeJSONAtomic(s.tagPath(repoHash, tag), tr); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) listRepositoriesLocked() ([]string, error) {
	entries, err := os.ReadDir(s.reposDir())
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	repos := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !isValidHex64(e.Name()) {
			continue
		}
		tags, err := s.readRepoTagsLocked(e.Name())
		if err != nil || len(tags) == 0 {
			continue
		}
		name, err := s.readRepoNameLocked(e.Name())
		if err != nil || name == "" {
			continue
		}
		repos = append(repos, name)
	}
	sort.Strings(repos)
	return repos, nil
}

func isUnknownPlatform(p *ociPlatform) bool {
	if p == nil {
		return false
	}
	return strings.EqualFold(p.OS, "unknown") && strings.EqualFold(p.Architecture, "unknown")
}

func isAttestationDescriptor(d ociDescriptor) bool {
	if d.Annotations != nil && d.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
		return true
	}
	return isUnknownPlatform(d.Platform)
}

func formatPlatform(osName, arch, variant string) string {
	osName = strings.TrimSpace(osName)
	arch = strings.TrimSpace(arch)
	variant = strings.TrimSpace(variant)
	if osName == "" || arch == "" {
		return ""
	}
	if strings.EqualFold(osName, "unknown") && strings.EqualFold(arch, "unknown") {
		return ""
	}
	if variant != "" {
		return osName + "/" + arch + "/" + variant
	}
	return osName + "/" + arch
}

func (s *Server) readConfigPlatformLocked(cfgHex string) string {
	f, err := os.Open(s.blobPath(cfgHex))
	if err != nil {
		return ""
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxConfigBlobSize))
	if err != nil {
		return ""
	}
	var cfg ociImageConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return formatPlatform(cfg.OS, cfg.Architecture, cfg.Variant)
}

func (s *Server) inspectImageLocked(repoHash, rootDigestHex string) (size int64, platforms []string) {
	seenManifests := make(map[string]bool)
	seenBlobs := make(map[string]bool)
	platSet := make(map[string]struct{})

	addBlob := func(d ociDescriptor) {
		hexPart, ok := parseDigest(d.Digest)
		if !ok || seenBlobs[hexPart] {
			return
		}
		seenBlobs[hexPart] = true
		if info, err := os.Stat(s.blobPath(hexPart)); err == nil {
			size += info.Size()
		} else if d.Size > 0 {
			size += d.Size
		}
	}

	var walk func(hexPart string, skipPlatform bool)
	walk = func(hexPart string, skipPlatform bool) {
		if seenManifests[hexPart] {
			return
		}
		seenManifests[hexPart] = true

		raw, err := os.ReadFile(s.revisionPath(repoHash, hexPart))
		if err != nil {
			// Fallback if a child descriptor referenced a raw blob rather than a manifest revision.
			if !seenBlobs[hexPart] {
				if info, serr := os.Stat(s.blobPath(hexPart)); serr == nil {
					seenBlobs[hexPart] = true
					size += info.Size()
				}
			}
			return
		}
		size += int64(len(raw))

		var m ociManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return
		}

		if len(m.Manifests) > 0 {
			for _, child := range m.Manifests {
				childHex, ok := parseDigest(child.Digest)
				if !ok {
					continue
				}
				if isAttestationDescriptor(child) {
					walk(childHex, true)
					continue
				}
				childSkipPlatform := skipPlatform
				if !skipPlatform && child.Platform != nil {
					if p := formatPlatform(child.Platform.OS, child.Platform.Architecture, child.Platform.Variant); p != "" {
						platSet[p] = struct{}{}
						childSkipPlatform = true
					}
				}
				walk(childHex, childSkipPlatform)
			}
		}

		if m.Config != nil && m.Config.Digest != "" {
			addBlob(*m.Config)
			if !skipPlatform {
				if cfgHex, ok := parseDigest(m.Config.Digest); ok {
					if p := s.readConfigPlatformLocked(cfgHex); p != "" {
						platSet[p] = struct{}{}
					}
				}
			}
		}
		for _, layer := range m.Layers {
			addBlob(layer)
		}
	}

	walk(rootDigestHex, false)

	if len(platSet) > 0 {
		platforms = make([]string, 0, len(platSet))
		for p := range platSet {
			platforms = append(platforms, p)
		}
		sort.Strings(platforms)
	}
	return size, platforms
}

type repoManifestKey struct {
	repoHash  string
	digestHex string
}

func (s *Server) collectReachableLocked() (liveManifests map[repoManifestKey]bool, liveBlobs map[string]bool, imageCount int, err error) {
	liveManifests = make(map[repoManifestKey]bool)
	liveBlobs = make(map[string]bool)

	repoEntries, err := os.ReadDir(s.reposDir())
	if errors.Is(err, fs.ErrNotExist) {
		return liveManifests, liveBlobs, 0, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}

	var walk func(repoHash, digestHex string)
	walk = func(repoHash, digestHex string) {
		key := repoManifestKey{repoHash: repoHash, digestHex: digestHex}
		if liveManifests[key] {
			return
		}
		liveManifests[key] = true

		raw, err := os.ReadFile(s.revisionPath(repoHash, digestHex))
		if err != nil {
			return
		}
		var m ociManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return
		}
		if m.Config != nil {
			if cfgHex, ok := parseDigest(m.Config.Digest); ok {
				liveBlobs[cfgHex] = true
			}
		}
		for _, layer := range m.Layers {
			if layerHex, ok := parseDigest(layer.Digest); ok {
				liveBlobs[layerHex] = true
			}
		}
		for _, child := range m.Manifests {
			if childHex, ok := parseDigest(child.Digest); ok {
				if s.manifestExistsLocked(repoHash, childHex) {
					walk(repoHash, childHex)
				} else if s.blobExistsLocked(childHex) {
					liveBlobs[childHex] = true
				}
			}
		}
	}

	for _, re := range repoEntries {
		if !re.IsDir() || !isValidHex64(re.Name()) {
			continue
		}
		repoHash := re.Name()
		tags, err := s.readRepoTagsLocked(repoHash)
		if err != nil {
			return nil, nil, 0, err
		}
		for _, t := range tags {
			imageCount++
			if hexPart, ok := parseDigest(t.Digest); ok {
				walk(repoHash, hexPart)
			}
		}
	}
	return liveManifests, liveBlobs, imageCount, nil
}

// List returns all tagged images currently stored in the registry, working
// whether the server is running or stopped.
func (s *Server) List() ([]ImageEntry, error) {
	if s.dir == "" {
		return nil, errNoDir
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	repoEntries, err := os.ReadDir(s.reposDir())
	if errors.Is(err, fs.ErrNotExist) {
		return []ImageEntry{}, nil
	}
	if err != nil {
		return nil, err
	}

	entries := make([]ImageEntry, 0)
	for _, re := range repoEntries {
		if !re.IsDir() || !isValidHex64(re.Name()) {
			continue
		}
		repoHash := re.Name()
		repoName, err := s.readRepoNameLocked(repoHash)
		if err != nil || repoName == "" {
			continue
		}
		tags, err := s.readRepoTagsLocked(repoHash)
		if err != nil {
			return nil, err
		}
		for _, tr := range tags {
			hexPart, ok := parseDigest(tr.Digest)
			if !ok {
				continue
			}
			meta, metaErr := s.readManifestMetaLocked(repoHash, hexPart)
			mediaType := meta.MediaType
			if mediaType == "" {
				mediaType = mediaTypeOCIImageManifest
			}
			updatedAt := tr.UpdatedAt
			if updatedAt.IsZero() && metaErr == nil {
				updatedAt = meta.UpdatedAt
			}
			size, platforms := s.inspectImageLocked(repoHash, hexPart)
			entries = append(entries, ImageEntry{
				Repository: repoName,
				Tag:        tr.Tag,
				Digest:     tr.Digest,
				MediaType:  mediaType,
				Size:       size,
				Platforms:  platforms,
				UpdatedAt:  updatedAt,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].UpdatedAt.Equal(entries[j].UpdatedAt) {
			return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
		}
		if entries[i].Repository != entries[j].Repository {
			return entries[i].Repository < entries[j].Repository
		}
		return entries[i].Tag < entries[j].Tag
	})
	return entries, nil
}

// DeleteTag removes the tag from repo and immediately runs garbage collection
// to reclaim any manifests and blobs that are no longer reachable.
func (s *Server) DeleteTag(repo, tag string) (GCResult, error) {
	if s.dir == "" {
		return GCResult{}, errNoDir
	}
	if !validRepoName(repo) {
		return GCResult{}, fmt.Errorf("localreg: invalid repository name %q", repo)
	}
	if !validTag(tag) {
		return GCResult{}, fmt.Errorf("localreg: invalid tag %q", tag)
	}

	s.mu.Lock()
	repoHash := sha256HexString(repo)
	if err := os.Remove(s.tagPath(repoHash, tag)); err != nil {
		s.mu.Unlock()
		if errors.Is(err, fs.ErrNotExist) {
			return GCResult{}, fmt.Errorf("localreg: image %s:%s not found: %w", repo, tag, fs.ErrNotExist)
		}
		return GCResult{}, err
	}
	res, err := s.gcLocked()
	s.mu.Unlock()

	s.notifyChange()
	return res, err
}

// GC removes all manifests and blobs not reachable from any existing tag,
// prunes empty repository directories, and never touches active upload staging files.
func (s *Server) GC() (GCResult, error) {
	if s.dir == "" {
		return GCResult{}, errNoDir
	}

	s.mu.Lock()
	res, err := s.gcLocked()
	s.mu.Unlock()

	if err == nil && (res.RemovedBlobs > 0 || res.RemovedManifests > 0) {
		s.notifyChange()
	}
	return res, err
}

func (s *Server) gcLocked() (GCResult, error) {
	var res GCResult
	if _, err := os.Stat(s.dir); errors.Is(err, fs.ErrNotExist) {
		return res, nil
	} else if err != nil {
		return res, err
	}

	liveManifests, liveBlobs, _, err := s.collectReachableLocked()
	if err != nil {
		return res, err
	}

	// Sweep unreferenced manifest revisions and prune empty repository directories.
	if repoEntries, err := os.ReadDir(s.reposDir()); err == nil {
		for _, re := range repoEntries {
			if !re.IsDir() || !isValidHex64(re.Name()) {
				continue
			}
			repoHash := re.Name()
			remainingRevs := 0
			if revEntries, rerr := os.ReadDir(s.revisionsDir(repoHash)); rerr == nil {
				for _, fe := range revEntries {
					if fe.IsDir() || !isValidHex64(fe.Name()) {
						continue
					}
					hexPart := fe.Name()
					if liveManifests[repoManifestKey{repoHash: repoHash, digestHex: hexPart}] {
						remainingRevs++
						continue
					}
					revPath := s.revisionPath(repoHash, hexPart)
					var revSize int64
					if info, ierr := fe.Info(); ierr == nil {
						revSize = info.Size()
					}
					if rmErr := os.Remove(revPath); rmErr == nil {
						_ = os.Remove(s.revisionMetaPath(repoHash, hexPart))
						res.RemovedManifests++
						res.ReclaimedBytes += revSize
					} else if !errors.Is(rmErr, fs.ErrNotExist) {
						remainingRevs++
					}
				}
			}

			tags, terr := s.readRepoTagsLocked(repoHash)
			if terr == nil && len(tags) == 0 && remainingRevs == 0 {
				_ = os.RemoveAll(s.repoDir(repoHash))
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}

	// Sweep unreferenced blobs (never touches uploads/).
	if blobEntries, err := os.ReadDir(s.blobsDir()); err == nil {
		for _, be := range blobEntries {
			if be.IsDir() || !isValidHex64(be.Name()) {
				continue
			}
			hexPart := be.Name()
			if liveBlobs[hexPart] {
				continue
			}
			var blobSize int64
			if info, ierr := be.Info(); ierr == nil {
				blobSize = info.Size()
			}
			if rmErr := os.Remove(s.blobPath(hexPart)); rmErr == nil {
				res.RemovedBlobs++
				res.ReclaimedBytes += blobSize
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}

	return res, nil
}

// DiskUsage reports total and reclaimable byte counts alongside image and blob
// counts, working whether the server is running or stopped.
func (s *Server) DiskUsage() (DiskStats, error) {
	if s.dir == "" {
		return DiskStats{}, errNoDir
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var stats DiskStats
	if _, err := os.Stat(s.dir); errors.Is(err, fs.ErrNotExist) {
		return stats, nil
	} else if err != nil {
		return stats, err
	}

	liveManifests, liveBlobs, imageCount, err := s.collectReachableLocked()
	if err != nil {
		return stats, err
	}
	stats.ImageCount = imageCount

	// Count all manifest revisions.
	if repoEntries, err := os.ReadDir(s.reposDir()); err == nil {
		for _, re := range repoEntries {
			if !re.IsDir() || !isValidHex64(re.Name()) {
				continue
			}
			repoHash := re.Name()
			if revEntries, rerr := os.ReadDir(s.revisionsDir(repoHash)); rerr == nil {
				for _, fe := range revEntries {
					if fe.IsDir() || !isValidHex64(fe.Name()) {
						continue
					}
					info, ierr := fe.Info()
					if ierr != nil {
						continue
					}
					sz := info.Size()
					stats.TotalBytes += sz
					if !liveManifests[repoManifestKey{repoHash: repoHash, digestHex: fe.Name()}] {
						stats.ReclaimableBytes += sz
					}
				}
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return stats, err
	}

	// Count all blobs.
	if blobEntries, err := os.ReadDir(s.blobsDir()); err == nil {
		for _, be := range blobEntries {
			if be.IsDir() || !isValidHex64(be.Name()) {
				continue
			}
			info, ierr := be.Info()
			if ierr != nil {
				continue
			}
			sz := info.Size()
			stats.BlobCount++
			stats.TotalBytes += sz
			if !liveBlobs[be.Name()] {
				stats.ReclaimableBytes += sz
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return stats, err
	}

	return stats, nil
}
