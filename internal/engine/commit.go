package engine

// commit (POST /commit, docs/design/sandbox-engine.md §9.4): the
// container's changes become one new layer on its image. The engine loads
// the new image as an OCI image layout that holds only that layer, the
// config and the manifest: rungv's load takes the image's other layers
// from its cache.

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// commitRunConfig is an image config's "config", as docker commit writes it.
type commitRunConfig struct {
	User         string              `json:"User,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint"`
	Cmd          []string            `json:"Cmd"`
	Healthcheck  json.RawMessage     `json:"Healthcheck,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
}

// commitConfig is the run config of the new image, as dockerd makes it:
// the request's config, the changes applied to it, then what it leaves
// unset taken from the container's config.
func commitConfig(container ContainerConfig, req ContainerConfig, changes []string) (ContainerConfig, error) {
	cfg := req.clone()
	if err := applyChanges(&cfg, changes); err != nil {
		return ContainerConfig{}, err
	}
	mergeCommitConfig(&cfg, container)
	return cfg, nil
}

// mergeCommitConfig fills what cfg leaves unset from the container's
// config, as dockerd's merge does: env and labels by key, exposed ports and
// volumes by union, the command only when cfg sets neither it nor an
// entrypoint.
func mergeCommitConfig(cfg *ContainerConfig, from ContainerConfig) {
	if cfg.User == "" {
		cfg.User = from.User
	}
	if len(cfg.ExposedPorts) == 0 {
		cfg.ExposedPorts = maps.Clone(from.ExposedPorts)
	} else {
		for p := range from.ExposedPorts {
			cfg.ExposedPorts[p] = struct{}{}
		}
	}
	if len(cfg.Env) == 0 {
		cfg.Env = slices.Clone(from.Env)
	} else {
		have := map[string]bool{}
		for _, kv := range cfg.Env {
			k, _, _ := strings.Cut(kv, "=")
			have[k] = true
		}
		for _, kv := range from.Env {
			if k, _, _ := strings.Cut(kv, "="); !have[k] {
				cfg.Env = append(cfg.Env, kv)
			}
		}
	}
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	for k, v := range from.Labels {
		if _, ok := cfg.Labels[k]; !ok {
			cfg.Labels[k] = v
		}
	}
	if len(cfg.Entrypoint) == 0 && len(cfg.Cmd) == 0 {
		cfg.Entrypoint = slices.Clone(from.Entrypoint)
		cfg.Cmd = slices.Clone(from.Cmd)
	}
	if len(cfg.Healthcheck) == 0 {
		cfg.Healthcheck = slices.Clone(from.Healthcheck)
	}
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = from.WorkingDir
	}
	if len(cfg.Volumes) == 0 {
		cfg.Volumes = maps.Clone(from.Volumes)
	} else {
		for v := range from.Volumes {
			cfg.Volumes[v] = struct{}{}
		}
	}
	if cfg.StopSignal == "" {
		cfg.StopSignal = from.StopSignal
	}
}

// applyChanges applies docker commit's --change instructions, the
// Dockerfile instructions commit allows, to cfg. A value may hold several
// lines.
func applyChanges(cfg *ContainerConfig, changes []string) error {
	cmdSet := false
	for _, c := range changes {
		for line := range strings.SplitSeq(c, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			word, rest, _ := strings.Cut(line, " ")
			rest = strings.TrimSpace(rest)
			switch strings.ToUpper(word) {
			case "CMD":
				cfg.Cmd = commandForm(rest)
				cmdSet = true
			case "ENTRYPOINT":
				cfg.Entrypoint = commandForm(rest)
				if !cmdSet {
					cfg.Cmd = nil
				}
			case "ENV":
				kvs, err := keyValues(rest, true)
				if err != nil {
					return errInvalid("%s: %v", line, err)
				}
				for _, kv := range kvs {
					cfg.Env = setEnv(cfg.Env, kv[0], kv[1])
				}
			case "LABEL":
				kvs, err := keyValues(rest, false)
				if err != nil {
					return errInvalid("%s: %v", line, err)
				}
				if cfg.Labels == nil {
					cfg.Labels = map[string]string{}
				}
				for _, kv := range kvs {
					cfg.Labels[kv[0]] = kv[1]
				}
			case "EXPOSE":
				if cfg.ExposedPorts == nil {
					cfg.ExposedPorts = map[string]struct{}{}
				}
				for _, p := range strings.Fields(rest) {
					if !strings.Contains(p, "/") {
						p += "/tcp"
					}
					cfg.ExposedPorts[strings.ToLower(p)] = struct{}{}
				}
			case "VOLUME":
				if cfg.Volumes == nil {
					cfg.Volumes = map[string]struct{}{}
				}
				vols := strings.Fields(rest)
				if a, ok := jsonArray(rest); ok {
					vols = a
				}
				for _, v := range vols {
					cfg.Volumes[v] = struct{}{}
				}
			case "USER":
				cfg.User = rest
			case "WORKDIR":
				if !path.IsAbs(rest) {
					rest = path.Join("/", cfg.WorkingDir, rest)
				}
				cfg.WorkingDir = path.Clean(rest)
			case "STOPSIGNAL":
				cfg.StopSignal = rest
			default:
				return errInvalid("%s is not a valid change command", word)
			}
		}
	}
	return nil
}

// commandForm reads CMD's or ENTRYPOINT's argument: a JSON array (exec
// form), or a command line for /bin/sh -c (shell form).
func commandForm(s string) []string {
	if a, ok := jsonArray(s); ok {
		return a
	}
	return []string{"/bin/sh", "-c", s}
}

func jsonArray(s string) ([]string, bool) {
	if !strings.HasPrefix(s, "[") {
		return nil, false
	}
	var a []string
	if json.Unmarshal([]byte(s), &a) != nil {
		return nil, false
	}
	return a, true
}

// keyValues reads ENV's or LABEL's arguments: key=value pairs, with
// double or single quotes and backslash escapes; ENV also takes the old
// form "key value...".
func keyValues(s string, oldForm bool) ([][2]string, error) {
	words, err := shellWords(s)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("needs at least one argument")
	}
	if oldForm && !strings.Contains(words[0], "=") {
		key, value, _ := strings.Cut(s, " ")
		return [][2]string{{key, strings.TrimSpace(value)}}, nil
	}
	var out [][2]string
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q is not key=value", w)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

// shellWords splits s at spaces outside quotes, removing the quotes.
func shellWords(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, quote, escaped := false, byte(0), false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			cur.WriteByte(ch)
			escaped = false
		case ch == '\\' && quote != '\'':
			escaped, in = true, true
		case quote != 0:
			if ch == quote {
				quote = 0
			} else {
				cur.WriteByte(ch)
			}
		case ch == '"' || ch == '\'':
			quote, in = ch, true
		case ch == ' ' || ch == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(ch)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

func setEnv(env []string, key, value string) []string {
	for i, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

// commitImageConfig is the new image's config: base's, with config as its
// run config, the new layer's diff ID, and a history entry.
func commitImageConfig(base []byte, cfg ContainerConfig, diffID string, created time.Time, author, comment, createdBy string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("the image's config: %w", err)
	}
	var rootfs struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	}
	if raw, ok := doc["rootfs"]; ok {
		if err := json.Unmarshal(raw, &rootfs); err != nil {
			return nil, fmt.Errorf("the image's rootfs: %w", err)
		}
	}
	rootfs.Type = "layers"
	rootfs.DiffIDs = append(rootfs.DiffIDs, diffID)

	var history []json.RawMessage
	if raw, ok := doc["history"]; ok {
		if err := json.Unmarshal(raw, &history); err != nil {
			return nil, fmt.Errorf("the image's history: %w", err)
		}
	}
	entry := map[string]string{"created": created.UTC().Format(time.RFC3339Nano), "created_by": createdBy}
	if author != "" {
		entry["author"] = author
	}
	if comment != "" {
		entry["comment"] = comment
	}
	h, _ := json.Marshal(entry)
	history = append(history, h)

	run := commitRunConfig{
		User: cfg.User, ExposedPorts: cfg.ExposedPorts, Env: cfg.Env, Entrypoint: cfg.Entrypoint,
		Cmd: cfg.Cmd, Healthcheck: cfg.Healthcheck, Volumes: cfg.Volumes, WorkingDir: cfg.WorkingDir,
		Labels: cfg.Labels, StopSignal: cfg.StopSignal,
	}
	set := func(k string, v any) {
		b, _ := json.Marshal(v)
		doc[k] = b
	}
	set("config", run)
	set("rootfs", rootfs)
	set("history", history)
	set("created", created.UTC().Format(time.RFC3339Nano))
	if author != "" {
		set("author", author)
	} else {
		delete(doc, "author")
	}
	return json.Marshal(doc)
}

// commitArchive writes the new image as an OCI image layout: its index,
// manifest, config and new layer (uncompressed, so its digest is its diff
// ID). The manifest lists the image's other layers by the descriptors the
// cache's manifest has, whose digests are the keys rungv's load finds them
// under: it skips a cached layer the archive lacks.
func commitArchive(w io.Writer, config []byte, base []json.RawMessage, layer *os.File, layerSize int64, diffID, repoTag string) error {
	tw := tar.NewWriter(w)
	add := func(name string, size int64, body io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: size}); err != nil {
			return err
		}
		_, err := io.CopyN(tw, body, size)
		return err
	}
	blob := func(b []byte) (string, error) {
		sum := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(sum[:])
		return d, add("blobs/sha256/"+hex.EncodeToString(sum[:]), int64(len(b)), bytes.NewReader(b))
	}
	type descriptor struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}

	if err := add("oci-layout", int64(len(`{"imageLayoutVersion":"1.0.0"}`)), strings.NewReader(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	if _, err := layer.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := add("blobs/sha256/"+strings.TrimPrefix(diffID, "sha256:"), layerSize, layer); err != nil {
		return err
	}
	cfgDigest, err := blob(config)
	if err != nil {
		return err
	}
	newLayer, _ := json.Marshal(descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: diffID, Size: layerSize})
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfgDigest, Size: int64(len(config))},
		"layers":        append(slices.Clone(base), newLayer),
	})
	manDigest, err := blob(manifest)
	if err != nil {
		return err
	}
	desc := descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manDigest, Size: int64(len(manifest))}
	if repoTag != "" {
		desc.Annotations = map[string]string{"io.containerd.image.name": repoTag}
	}
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []descriptor{desc},
	})
	if err := add("index.json", int64(len(index)), bytes.NewReader(index)); err != nil {
		return err
	}
	return tw.Close()
}

// commitTarget is the tag commit gives the image: repo with tag, or
// "latest"; none without repo.
func commitTarget(repo, tag string) (string, error) {
	if repo == "" {
		if tag != "" {
			return "", errInvalid("a tag needs a repository")
		}
		return "", nil
	}
	if err := validateImageRef(repo); err != nil {
		return "", err
	}
	if repo != strings.ToLower(repo) {
		return "", errInvalid("invalid reference format: repository name (%s) must be lowercase", repo)
	}
	switch {
	case tag != "":
		if err := validateImageRef(tag); err != nil || strings.ContainsAny(tag, ":/@") {
			return "", errInvalid("invalid tag format")
		}
		return repo + ":" + tag, nil
	case strings.LastIndex(repo, ":") > strings.LastIndex(repo, "/"):
		return repo, nil
	}
	return repo + ":latest", nil
}

type commitResponse struct {
	ID string `json:"Id"`
}

func (e *Engine) commit(w http.ResponseWriter, r *http.Request) {
	if !e.runner.Features().Control {
		writeError(w, r, errNotSupported("Committing a container (it needs rungv 0.20)"))
		return
	}
	q := r.URL.Query()
	c, err := e.lookup(q.Get("container"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	target, err := commitTarget(strings.TrimSpace(q.Get("repo")), strings.TrimSpace(q.Get("tag")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req ContainerConfig
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	cfg, err := commitConfig(c.Config, req, q["changes"])
	if err != nil {
		writeError(w, r, err)
		return
	}
	// dockerd pauses by default; pause=0 does not.
	pause := q.Get("pause") == "" || queryBool(r, "pause")

	img, err := e.openImage(r.Context(), c.ImageID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	base, baseLayers := img.Config, img.LayerDescriptors
	img.Close()

	layer, err := e.tempFile("commit-")
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer func() { layer.Close(); os.Remove(layer.Name()) }()
	sum := sha256.New()
	var size int64
	err = e.withUpper(r.Context(), c, pause, "commit it", func(upper io.Reader) error {
		n, err := io.Copy(io.MultiWriter(layer, sum), upper)
		size = n
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	diffID := "sha256:" + hex.EncodeToString(sum.Sum(nil))
	createdBy := strings.Join(append(slices.Clone(c.Config.Entrypoint), c.Config.Cmd...), " ")
	config, err := commitImageConfig(base, cfg, diffID, time.Now(), q.Get("author"), q.Get("comment"), createdBy)
	if err != nil {
		writeError(w, r, err)
		return
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(commitArchive(pw, config, baseLayers, layer, size, diffID, target)) }()
	err = e.runner.LoadImages(r.Context(), pr, func(string) {})
	pr.Close()
	if err != nil {
		writeError(w, r, err)
		return
	}
	cfgSum := sha256.Sum256(config)
	id := "sha256:" + hex.EncodeToString(cfgSum[:])

	attrs := containerAttrs(&c)
	attrs["imageID"] = id
	if target != "" {
		attrs["imageRef"] = target
	}
	if comment := q.Get("comment"); comment != "" {
		attrs["comment"] = comment
	}
	e.emit(Event{Type: "container", Action: "commit", ID: c.ID, Attributes: attrs})
	if target != "" {
		e.emit(Event{Type: "image", Action: "tag", ID: id, Attributes: map[string]string{"name": target}})
	}
	e.notify()
	writeJSON(w, http.StatusCreated, commitResponse{ID: id})
}
