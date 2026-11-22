package engine

// Owner: workstream C (images, volumes, networks, system).
// Workstream C may add files named image_*.go, volume_*.go, network_*.go,
// system_*.go and events_*.go.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// imageState tracks lightweight cache hints for image listings (such as
// whether the foreign-architecture namespace needs to be queried).
type imageState struct {
	mu             sync.Mutex
	foreignChecked bool
	hasForeign     bool
}

// ImageInspect is rungv inspect of one image, which follows docker image
// inspect.
type ImageInspect struct {
	ID           string      `json:"Id"` // sha256:<64 hex>
	RepoTags     []string    `json:"RepoTags"`
	RepoDigests  []string    `json:"RepoDigests"`
	Created      time.Time   `json:"Created"`
	Architecture string      `json:"Architecture"`
	Os           string      `json:"Os"`
	Config       ImageConfig `json:"Config"`
	RootFS       struct {
		Type   string   `json:"Type"`
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
	Size int64 `json:"Size"`
	// ResolvedRef is the reference that rungv's cache knows the image by,
	// the one that `rungv run -pull never` accepts. Set by imageConfig on
	// every success, even when it equals the input ref.
	ResolvedRef string `json:"-"`
}

// ImageConfig is the run config of an image, what a container's create
// merges with its own.
type ImageConfig struct {
	Env          []string            `json:"Env"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	WorkingDir   string              `json:"WorkingDir"`
	User         string              `json:"User"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
}

type imageSummary struct {
	ID          string                 `json:"Id"`
	ParentID    string                 `json:"ParentId"`
	RepoTags    []string               `json:"RepoTags"`
	RepoDigests []string               `json:"RepoDigests"`
	Created     int64                  `json:"Created"`
	Size        int64                  `json:"Size"`
	SharedSize  int64                  `json:"SharedSize"`
	Labels      map[string]string      `json:"Labels"`
	Containers  int64                  `json:"Containers"`
	Manifests   []imageManifestSummary `json:"Manifests,omitempty"`
	arch        string
}

type imageInspectResponse struct {
	ID           string      `json:"Id"`
	RepoTags     []string    `json:"RepoTags"`
	RepoDigests  []string    `json:"RepoDigests"`
	Parent       string      `json:"Parent"`
	Comment      string      `json:"Comment"`
	Created      time.Time   `json:"Created"`
	Architecture string      `json:"Architecture"`
	Os           string      `json:"Os"`
	Size         int64       `json:"Size"`
	Config       ImageConfig `json:"Config"`
	RootFS       struct {
		Type   string   `json:"Type"`
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
	Metadata struct {
		LastTagTime time.Time `json:"LastTagTime"`
	} `json:"Metadata"`
}

// ---- seams: called by workstream B; frozen ----

// imageConfig inspects the cached host-architecture image ref. An image that
// is not cached is a 404 "No such image: ref", which makes the CLI pull it.
func (e *Engine) imageConfig(ctx context.Context, ref string) (ImageInspect, error) {
	return e.lookupImageInArch(ctx, hostImageArch, ref)
}

// resolvedCacheRef picks the reference that rungv run -pull never accepts for
// img. rungv inspect accepts short/full image IDs, but rungv run requires a
// cached RepoTag or RepoDigest.
func resolvedCacheRef(matched string, img ImageInspect) string {
	if isImageIDRef(matched, img.ID) {
		if len(img.RepoTags) > 0 && img.RepoTags[0] != "" {
			return img.RepoTags[0]
		}
		if len(img.RepoDigests) > 0 && img.RepoDigests[0] != "" {
			return img.RepoDigests[0]
		}
		return img.ID
	}
	norm := normalizeImageRef(matched)
	for _, t := range img.RepoTags {
		if t == matched || normalizeImageRef(t) == norm {
			return t
		}
	}
	for _, d := range img.RepoDigests {
		if d == matched {
			return d
		}
	}
	return matched
}

func validateImageRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return errInvalid("invalid reference format")
	}
	for i := range len(ref) {
		if ref[i] <= 0x20 || ref[i] == 0x7f {
			return errInvalid("invalid reference format")
		}
	}
	return nil
}

func validateImagePlatform(platform string) error {
	if platform == "" {
		return nil
	}
	if strings.HasPrefix(platform, "-") {
		return errInvalid("invalid platform: %q", platform)
	}
	for i := range len(platform) {
		if platform[i] <= 0x20 || platform[i] == 0x7f {
			return errInvalid("invalid platform: %q", platform)
		}
	}
	return nil
}

func (e *Engine) isKeptImage(id string, repoTags, repoDigests []string) bool {
	if e.cfg.KeepImages == nil {
		return false
	}
	for _, keep := range e.cfg.KeepImages() {
		if imageMatchesRef(keep, id, repoTags, repoDigests) {
			return true
		}
	}
	return false
}

// imageMatchesRef reports whether ref names the image identified by id,
// repoTags and repoDigests. It matches full IDs, ID prefixes (with or without
// sha256:), tag references (normalized), and digest references (repo@sha256:...,
// repo:tag@sha256:..., or bare sha256:...).
func imageMatchesRef(ref, id string, repoTags, repoDigests []string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if id != "" && strings.EqualFold(ref, id) {
		return true
	}

	imgHex := strings.TrimPrefix(strings.ToLower(id), "sha256:")
	lowerRef := strings.ToLower(ref)
	if strings.HasPrefix(lowerRef, "sha256:") {
		needle := strings.TrimPrefix(lowerRef, "sha256:")
		if imgHex != "" && len(needle) >= 3 && isHexString(needle) && strings.HasPrefix(imgHex, needle) {
			return true
		}
		for _, rd := range repoDigests {
			if _, rdDigest, ok := strings.Cut(rd, "@"); ok && strings.EqualFold(strings.TrimSpace(rdDigest), ref) {
				return true
			}
		}
		return false
	}
	if !strings.ContainsAny(ref, "/:@") && imgHex != "" && len(lowerRef) >= 3 && isHexString(lowerRef) && strings.HasPrefix(imgHex, lowerRef) {
		return true
	}

	if kRepo, kDigest, ok := strings.Cut(ref, "@"); ok {
		kDigest = strings.ToLower(strings.TrimSpace(kDigest))
		if kDigest == "" {
			return false
		}
		if id != "" && strings.EqualFold(id, kDigest) {
			return true
		}
		kRepo = strings.TrimSpace(kRepo)
		if i := strings.LastIndex(kRepo, ":"); i > strings.LastIndex(kRepo, "/") {
			kRepo = kRepo[:i]
		}
		kRepo = stripDefaultRegistry(kRepo)
		for _, rd := range repoDigests {
			rdRepo, rdDigest, ok := strings.Cut(rd, "@")
			if !ok || !strings.EqualFold(strings.TrimSpace(rdDigest), kDigest) {
				continue
			}
			if kRepo == "" || stripDefaultRegistry(strings.TrimSpace(rdRepo)) == kRepo {
				return true
			}
		}
		return false
	}

	norm := normalizeImageRef(ref)
	for _, t := range repoTags {
		if t == ref || normalizeImageRef(t) == norm {
			return true
		}
	}
	return false
}

func imageRefAlternatives(ref string) []string {
	var alts []string
	seen := map[string]bool{ref: true}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			alts = append(alts, s)
		}
	}
	stripped := stripDefaultRegistry(ref)
	add(stripped)
	if !strings.Contains(stripped, "@") && strings.LastIndex(stripped, ":") <= strings.LastIndex(stripped, "/") {
		add(stripped + ":latest")
	}
	if !strings.Contains(ref, "@") && strings.LastIndex(ref, ":") <= strings.LastIndex(ref, "/") {
		add(ref + ":latest")
	}
	return alts
}

func stripDefaultRegistry(ref string) string {
	for _, p := range []string{"docker.io/library/", "index.docker.io/library/", "docker.io/", "index.docker.io/"} {
		if rest, ok := strings.CutPrefix(ref, p); ok {
			return rest
		}
	}
	if rest, ok := strings.CutPrefix(ref, "library/"); ok && !strings.Contains(rest, "/") {
		return rest
	}
	return ref
}

func normalizeImageRef(ref string) string {
	s := stripDefaultRegistry(strings.TrimSpace(ref))
	if s == "" || strings.HasPrefix(s, "sha256:") || strings.Contains(s, "@") {
		return s
	}
	if strings.LastIndex(s, ":") <= strings.LastIndex(s, "/") {
		return s + ":latest"
	}
	return s
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// ---- handlers ----

func (e *Engine) imageList(w http.ResponseWriter, r *http.Request) {
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	rows, err := e.buildImageSummaries(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Resolve before/since anchor timestamps if requested.
	var beforeTime, sinceTime time.Time
	hasBefore := f.has("before")
	hasSince := f.has("since")
	if hasBefore {
		if img, _, err := e.lookupImage(r.Context(), f["before"][0]); err == nil {
			beforeTime = img.Created
		} else {
			writeError(w, r, err)
			return
		}
	}
	if hasSince {
		if img, _, err := e.lookupImage(r.Context(), f["since"][0]); err == nil {
			sinceTime = img.Created
		} else {
			writeError(w, r, err)
			return
		}
	}

	out := make([]imageSummary, 0, len(rows))
	for _, row := range rows {
		if f.has("dangling") {
			isDangling := len(row.RepoTags) == 0
			if !f.matchAny("dangling", func(v string) bool {
				want := strings.EqualFold(v, "true") || v == "1"
				return isDangling == want
			}) {
				continue
			}
		}
		if f.has("reference") {
			if !f.matchAny("reference", func(pattern string) bool {
				return matchImageReference(row.RepoTags, pattern)
			}) {
				continue
			}
		}
		if !f.matchLabels(row.Labels) {
			continue
		}
		if hasBefore && !time.Unix(row.Created, 0).Before(beforeTime) {
			continue
		}
		if hasSince && !time.Unix(row.Created, 0).After(sinceTime) {
			continue
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) buildImageSummaries(ctx context.Context) ([]imageSummary, error) {
	ctrCounts := map[string]int64{}
	for _, c := range e.allContainers() {
		if c.ImageID != "" {
			ctrCounts[c.ImageID]++
		}
	}

	out, err := e.buildImageSummariesForArch(ctx, hostImageArch, ctrCounts)
	if err != nil {
		return nil, err
	}
	if _, ok := e.archRunner(); ok && e.shouldListForeignImages() {
		foreign := foreignImageArch()
		fRows, ferr := e.buildImageSummariesForArch(ctx, foreign, ctrCounts)
		if ferr != nil {
			e.logf("engine: list %s images: %v", foreign, ferr)
			e.recordForeignImagePresence(false)
		} else {
			e.recordForeignImagePresence(len(fRows) > 0)
			out = append(out, fRows...)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].arch < out[j].arch
	})
	return out, nil
}

func (e *Engine) buildImageSummariesForArch(ctx context.Context, arch string, ctrCounts map[string]int64) ([]imageSummary, error) {
	rows, err := e.imagesInArch(ctx, arch)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []imageSummary{}, nil
	}

	type grouped struct {
		id      string
		tags    []string
		digests []string
		row     rungv.Image
	}
	byID := map[string]*grouped{}
	var order []string
	for _, row := range rows {
		g, ok := byID[row.ID]
		if !ok {
			g = &grouped{id: row.ID, row: row}
			byID[row.ID] = g
			order = append(order, row.ID)
		}
		if row.Repository != "" && row.Repository != "<none>" && row.Tag != "" && row.Tag != "<none>" {
			tag := row.Repository + ":" + row.Tag
			if !slices.Contains(g.tags, tag) {
				g.tags = append(g.tags, tag)
			}
		}
		if row.Repository != "" && row.Repository != "<none>" && row.Digest != "" && row.Digest != "<none>" {
			dig := row.Repository + "@" + row.Digest
			if !slices.Contains(g.digests, dig) {
				g.digests = append(g.digests, dig)
			}
		}
	}

	inspected := map[string]ImageInspect{}
	if insList, err := e.inspectImagesInArch(ctx, arch, order...); err == nil {
		for _, ins := range insList {
			inspected[ins.ID] = ins
		}
	} else {
		// Fall back one by one in case one ID disappeared concurrently.
		for _, id := range order {
			if insList, err := e.inspectImagesInArch(ctx, arch, id); err == nil && len(insList) > 0 {
				inspected[insList[0].ID] = insList[0]
			}
		}
	}

	out := make([]imageSummary, 0, len(order))
	for _, id := range order {
		g := byID[id]
		ins, hasIns := inspected[id]
		tags := g.tags
		digests := g.digests
		var created int64
		var size int64
		var labels map[string]string
		imgArch := arch
		imgOS := "linux"
		if hasIns {
			if len(ins.RepoTags) > 0 {
				tags = slices.Clone(ins.RepoTags)
			}
			if len(ins.RepoDigests) > 0 {
				digests = slices.Clone(ins.RepoDigests)
			}
			if !ins.Created.IsZero() {
				created = ins.Created.Unix()
			}
			size = ins.Size
			labels = ins.Config.Labels
			if a := normalizeArch(ins.Architecture); a != "" {
				imgArch = a
			}
			if ins.Os != "" {
				imgOS = ins.Os
			}
		}
		if created == 0 {
			if t, err := parseRungvTime(g.row.CreatedAt); err == nil {
				created = t.Unix()
			}
		}
		if size == 0 {
			size = parseHumanSize(g.row.Size)
		}
		if tags == nil {
			tags = []string{}
		}
		if digests == nil {
			digests = []string{}
		}
		out = append(out, imageSummary{
			ID:          id,
			ParentID:    "",
			RepoTags:    tags,
			RepoDigests: digests,
			Created:     created,
			Size:        size,
			SharedSize:  -1,
			Labels:      labels,
			Containers:  ctrCounts[id],
			Manifests:   makeImageManifests(id, imgArch, imgOS, size),
			arch:        arch,
		})
	}
	return out, nil
}

func matchImageReference(tags []string, pattern string) bool {
	pat := stripDefaultRegistry(strings.TrimSpace(pattern))
	hasTag := strings.LastIndex(pat, ":") > strings.LastIndex(pat, "/")
	for _, rawTag := range tags {
		t := stripDefaultRegistry(rawTag)
		if t == pat || rawTag == pattern {
			return true
		}
		if !hasTag {
			repo, _, _ := strings.Cut(t, ":")
			if repo == pat {
				return true
			}
			if ok, _ := path.Match(pat, repo); ok {
				return true
			}
		}
		if ok, _ := path.Match(pat, t); ok {
			return true
		}
	}
	return false
}

func (e *Engine) imageCreate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("fromSrc")) != "" {
		writeError(w, r, errNotSupported("Importing an image"))
		return
	}
	fromImage := strings.TrimSpace(q.Get("fromImage"))
	if err := validateImageRef(fromImage); err != nil {
		writeError(w, r, err)
		return
	}
	tag := strings.TrimSpace(q.Get("tag"))
	if tag != "" {
		if err := validateImageRef(tag); err != nil {
			writeError(w, r, err)
			return
		}
	}
	platform := strings.TrimSpace(q.Get("platform"))
	if err := validateImagePlatform(platform); err != nil {
		writeError(w, r, err)
		return
	}

	ref := fromImage
	if tag != "" {
		if strings.HasPrefix(tag, "sha256:") {
			ref = fromImage + "@" + tag
		} else if !strings.Contains(fromImage, "@") && strings.LastIndex(fromImage, ":") <= strings.LastIndex(fromImage, "/") {
			ref = fromImage + ":" + tag
		}
	}
	if err := validateImageRef(ref); err != nil {
		writeError(w, r, err)
		return
	}

	rc := http.NewResponseController(w)
	var (
		mu         sync.Mutex
		started    bool
		sawStatus  bool
		writeFrame = func(v any) {
			b, err := json.Marshal(v)
			if err != nil {
				return
			}
			b = append(b, '\n')
			if !started {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			_, _ = w.Write(b)
			_ = rc.Flush()
		}
	)

	err := e.runner.Pull(r.Context(), ref, platform, func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(line, "Status: ") {
			sawStatus = true
		}
		writeFrame(pullProgressFrame(line))
	})

	mu.Lock()
	if err != nil {
		msg := err.Error()
		if !started {
			mu.Unlock()
			if isImageNotFoundErr(msg) {
				writeError(w, r, errNotFound("%s", msg))
				return
			}
			writeError(w, r, err)
			return
		}
		writeFrame(struct {
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
			Error string `json:"error"`
		}{
			ErrorDetail: struct {
				Message string `json:"message"`
			}{Message: msg},
			Error: msg,
		})
		mu.Unlock()
		return
	}
	if !sawStatus {
		writeFrame(map[string]string{
			"status": "Status: Downloaded newer image for " + ref,
		})
	}
	mu.Unlock()

	if reqArch := parsePlatformArch(platform); reqArch != "" && reqArch != hostImageArch {
		e.markForeignImagesPossible()
	}

	e.emit(Event{
		Type:       "image",
		Action:     "pull",
		ID:         ref,
		Attributes: map[string]string{"name": ref},
	})
	e.notify()
}

func pullProgressFrame(line string) map[string]string {
	if id, rest, ok := strings.Cut(line, ": "); ok && id != "" && !strings.ContainsAny(id, " \t") {
		switch strings.ToLower(id) {
		case "status", "digest", "rungv", "gvx", "error", "warning":
			return map[string]string{"status": line}
		}
		return map[string]string{"status": rest, "id": id}
	}
	return map[string]string{"status": line}
}

func isImageNotFoundErr(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "manifest unknown") ||
		strings.Contains(m, "name unknown") ||
		strings.Contains(m, "not found") ||
		strings.Contains(m, "404")
}

func (e *Engine) imageInspect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	img, _, err := e.lookupImageForPlatform(r.Context(), name, platform)
	if err != nil {
		writeError(w, r, err)
		return
	}
	tags := slices.Clone(img.RepoTags)
	if tags == nil {
		tags = []string{}
	}
	digests := slices.Clone(img.RepoDigests)
	if digests == nil {
		digests = []string{}
	}
	resp := imageInspectResponse{
		ID:           img.ID,
		RepoTags:     tags,
		RepoDigests:  digests,
		Created:      img.Created,
		Architecture: img.Architecture,
		Os:           img.Os,
		Size:         img.Size,
		Config:       img.Config,
		RootFS:       img.RootFS,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (e *Engine) imageTag(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("name")
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if err := validateImageRef(source); err != nil {
		writeError(w, r, err)
		return
	}
	if err := validateImageRef(repo); err != nil {
		writeError(w, r, err)
		return
	}
	if tag != "" {
		if err := validateImageRef(tag); err != nil {
			writeError(w, r, err)
			return
		}
	}
	img, arch, err := e.lookupImageForPlatform(r.Context(), source, platform)
	if err != nil {
		writeError(w, r, err)
		return
	}
	target := repo
	if tag != "" {
		target = repo + ":" + tag
	} else if strings.LastIndex(repo, ":") <= strings.LastIndex(repo, "/") {
		target = repo + ":latest"
	}
	if err := validateImageRef(target); err != nil {
		writeError(w, r, err)
		return
	}
	if err := e.tagImageInArch(r.Context(), arch, source, target); err != nil {
		if err2 := e.tagImageInArch(r.Context(), arch, img.ID, target); err2 != nil {
			writeError(w, r, err)
			return
		}
	}
	e.emit(Event{
		Type:       "image",
		Action:     "tag",
		ID:         img.ID,
		Attributes: map[string]string{"name": target},
	})
	e.notify()
	w.WriteHeader(http.StatusCreated)
}

func (e *Engine) imageDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	force := queryBool(r, "force")
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))

	img, arch, err := e.lookupImageForPlatform(r.Context(), name, platform)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Never delete an image used by the user's own GVX Desktop sandboxes,
	// even when force=1 is passed (e.g. from the UI's delete action).
	if arch == hostImageArch && e.isKeptImage(img.ID, img.RepoTags, img.RepoDigests) {
		writeError(w, r, errConflict(
			"conflict: unable to remove image %q - a GVX Desktop sandbox uses it",
			name,
		))
		return
	}
	if arch == hostImageArch && e.isBuilderImageProtected(img.ID, img.RepoTags, img.RepoDigests) {
		writeError(w, r, errConflict(
			"conflict: unable to remove image %q - the sandbox engine's builder is using it",
			name,
		))
		return
	}
	byID := isImageIDRef(name, img.ID)

	if !force {
		ctrs := e.allContainers()
		for _, c := range ctrs {
			if c.ImageID != img.ID {
				continue
			}
			// Allow untagging an unused extra alias when multiple tags remain.
			if !byID && len(img.RepoTags) > 1 && normalizeImageRef(c.Image) != normalizeImageRef(name) {
				continue
			}
			writeError(w, r, errConflict(
				"conflict: unable to remove repository reference %q (must force) - container %s is using its referenced image %s",
				name, shortHex(c.ID), shortHex(img.ID),
			))
			return
		}
	}

	removeRef := name
	if byID {
		removeRef = img.ID
	}
	if err := e.removeImageInArch(r.Context(), arch, removeRef, force); err != nil {
		// Retry with normalized ref if the client sent e.g. alpine without :latest.
		if !byID {
			if alt := normalizeImageRef(name); alt != removeRef {
				err = e.removeImageInArch(r.Context(), arch, alt, force)
			}
		}
		if err != nil {
			msg := err.Error()
			if strings.Contains(strings.ToLower(msg), "not found") {
				writeError(w, r, errNotFound("No such image: %s", name))
				return
			}
			if strings.Contains(strings.ToLower(msg), "multiple") || strings.Contains(strings.ToLower(msg), "conflict") {
				writeError(w, r, errConflict("%s", msg))
				return
			}
			writeError(w, r, err)
			return
		}
	}

	var result []map[string]string
	if byID {
		for _, t := range img.RepoTags {
			result = append(result, map[string]string{"Untagged": t})
			e.emit(Event{Type: "image", Action: "untag", ID: img.ID, Attributes: map[string]string{"name": t}})
		}
	} else {
		untagged := matchTagForName(img.RepoTags, name)
		result = append(result, map[string]string{"Untagged": untagged})
		e.emit(Event{Type: "image", Action: "untag", ID: img.ID, Attributes: map[string]string{"name": untagged}})
	}

	// Check whether the image itself was removed or still exists in the cache.
	deleted := false
	if _, err := e.inspectImagesInArch(r.Context(), arch, img.ID); err != nil {
		deleted = true
	} else if rows, err := e.imagesInArch(r.Context(), arch); err == nil {
		found := false
		for _, row := range rows {
			if row.ID == img.ID {
				found = true
				break
			}
		}
		if !found {
			deleted = true
		}
	}
	if deleted {
		result = append(result, map[string]string{"Deleted": img.ID})
		e.emit(Event{Type: "image", Action: "delete", ID: img.ID, Attributes: map[string]string{"name": img.ID}})
	}
	e.notify()
	writeJSON(w, http.StatusOK, result)
}

func isImageIDRef(ref, fullID string) bool {
	if ref == fullID {
		return true
	}
	r := strings.TrimPrefix(strings.ToLower(ref), "sha256:")
	f := strings.TrimPrefix(strings.ToLower(fullID), "sha256:")
	return len(r) >= 3 && isHexString(r) && strings.HasPrefix(f, r)
}

func matchTagForName(repoTags []string, name string) string {
	norm := normalizeImageRef(name)
	for _, t := range repoTags {
		if t == name || normalizeImageRef(t) == norm {
			return t
		}
	}
	return norm
}

func shortHex(id string) string {
	s := strings.TrimPrefix(id, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func (e *Engine) imageLoad(w http.ResponseWriter, r *http.Request) {
	quiet := queryBool(r, "quiet")
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()

	var (
		mu      sync.Mutex
		started bool
	)
	writeObj := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		b = append(b, '\n')
		if !started {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		_, _ = w.Write(b)
		_ = rc.Flush()
	}

	tmpFile, cleanup, err := e.spoolImageArchive(r.Body)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer cleanup()

	arches := detectArchiveArches(tmpFile)
	if len(arches) == 0 {
		arches = []string{hostImageArch}
	}

	onLine := func(line string) {
		if quiet {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		mu.Lock()
		writeObj(map[string]string{"stream": line + "\n"})
		mu.Unlock()
	}

	var loadErr error
	for _, arch := range arches {
		if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
			loadErr = err
			break
		}
		if err := e.loadImagesInArch(r.Context(), arch, tmpFile, onLine); err != nil {
			loadErr = err
			break
		}
	}
	if loadErr != nil {
		mu.Lock()
		if !started {
			mu.Unlock()
			writeError(w, r, loadErr)
			return
		}
		writeObj(struct {
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
			Error string `json:"error"`
		}{
			ErrorDetail: struct {
				Message string `json:"message"`
			}{Message: loadErr.Error()},
			Error: loadErr.Error(),
		})
		mu.Unlock()
		return
	}
	mu.Lock()
	if !started {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}
	mu.Unlock()
	e.emit(Event{Type: "image", Action: "load", ID: ""})
	e.notify()
}

type tarStreamWriter struct {
	w       http.ResponseWriter
	started bool
}

func (t *tarStreamWriter) Write(p []byte) (int, error) {
	if !t.started && len(p) > 0 {
		t.w.Header().Set("Content-Type", "application/x-tar")
		t.w.WriteHeader(http.StatusOK)
		t.started = true
	}
	return t.w.Write(p)
}

func (e *Engine) imageGet(w http.ResponseWriter, r *http.Request) {
	var refs []string
	if name := r.PathValue("name"); name != "" {
		refs = []string{name}
	} else {
		for _, n := range r.URL.Query()["names"] {
			if s := strings.TrimSpace(n); s != "" {
				refs = append(refs, s)
			}
		}
	}
	if len(refs) == 0 {
		writeError(w, r, errInvalid("at least one image name is required"))
		return
	}
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	arch := ""
	for _, ref := range refs {
		if err := validateImageRef(ref); err != nil {
			writeError(w, r, err)
			return
		}
		_, refArch, err := e.lookupImageForPlatform(r.Context(), ref, platform)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if arch == "" {
			arch = refArch
		} else if arch != refArch {
			writeError(w, r, errInvalid("cannot save images of mixed architectures in one archive"))
			return
		}
	}
	tw := &tarStreamWriter{w: w}
	if err := e.saveImagesInArch(r.Context(), arch, tw, refs...); err != nil {
		if !tw.started {
			writeError(w, r, err)
		}
		return
	}
	if !tw.started {
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
	}
}

func (e *Engine) imagePrune(w http.ResponseWriter, r *http.Request) {
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	danglingOnly := true
	if f.has("dangling") {
		danglingOnly = false
		for _, v := range f["dangling"] {
			if strings.EqualFold(v, "true") || v == "1" {
				danglingOnly = true
			}
		}
	}

	var untilTime time.Time
	if f.has("until") {
		t, err := parseEventTime(f["until"][0], time.Now().UTC())
		if err != nil {
			writeError(w, r, err)
			return
		}
		untilTime = t
	}

	type deleteItem struct {
		Untagged string `json:"Untagged,omitempty"`
		Deleted  string `json:"Deleted,omitempty"`
	}
	var deleted []deleteItem
	var reclaimed int64

	// Always prune image-by-image ourselves instead of calling rungv image prune,
	// which knows nothing of Config.KeepImages or container records.
	ctrs := e.allContainers()

	summaries, err := e.buildImageSummaries(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}

	for _, img := range summaries {
		if danglingOnly && len(img.RepoTags) > 0 {
			continue
		}
		if img.arch == hostImageArch && e.isKeptImage(img.ID, img.RepoTags, img.RepoDigests) {
			continue
		}
		if img.arch == hostImageArch && e.isBuilderImageProtected(img.ID, img.RepoTags, img.RepoDigests) {
			continue
		}
		inUse := false
		for _, c := range ctrs {
			if c.ImageID == img.ID || (img.arch == hostImageArch && imageMatchesRef(c.Image, img.ID, img.RepoTags, img.RepoDigests)) {
				inUse = true
				break
			}
		}
		if inUse {
			continue
		}
		if !f.matchLabels(img.Labels) {
			continue
		}
		if !untilTime.IsZero() && !time.Unix(img.Created, 0).Before(untilTime) {
			continue
		}
		if err := e.removeImageInArch(r.Context(), img.arch, img.ID, true); err != nil {
			continue
		}
		for _, t := range img.RepoTags {
			deleted = append(deleted, deleteItem{Untagged: t})
		}
		deleted = append(deleted, deleteItem{Deleted: img.ID})
		reclaimed += img.Size
		e.emit(Event{Type: "image", Action: "delete", ID: img.ID, Attributes: map[string]string{"name": img.ID}})
	}

	e.emit(Event{Type: "image", Action: "prune", Attributes: map[string]string{"reclaimed": formatInt64(reclaimed)}})
	e.notify()
	writeJSON(w, http.StatusOK, struct {
		ImagesDeleted  []deleteItem `json:"ImagesDeleted"`
		SpaceReclaimed int64        `json:"SpaceReclaimed"`
	}{ImagesDeleted: deleted, SpaceReclaimed: reclaimed})
}
