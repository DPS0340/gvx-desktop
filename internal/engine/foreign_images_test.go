package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func buildTarArchive(t *testing.T, gzipped bool, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var tw *tar.Writer
	var gzw *gzip.Writer
	if gzipped {
		gzw = gzip.NewWriter(&buf)
		tw = tar.NewWriter(gzw)
	} else {
		tw = tar.NewWriter(&buf)
	}
	for name, data := range files {
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gzw != nil {
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestForeignImagesListInspectTagDeleteSavePrune(t *testing.T) {
	te := newTestEngine(t)
	foreign := foreignImageArch()

	hostID := te.fake.addImage("app:1", ImageConfig{Cmd: []string{"/host"}})
	foreignSameTagID := te.fake.addForeignImage(foreign, "app:1", ImageConfig{Cmd: []string{"/foreign"}})
	foreignOnlyID := te.fake.addForeignImage(foreign, "gvxd-amd64:1", ImageConfig{Cmd: []string{"/amd64"}})

	// GET /images/json includes both namespaces and carries Manifests with platform arch.
	st, body := te.do(t, http.MethodGet, "/images/json", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /images/json = %d %s", st, body)
	}
	var list []imageSummary
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d images, want 3: %+v", len(list), list)
	}
	byID := map[string]imageSummary{}
	for _, row := range list {
		byID[row.ID] = row
		if len(row.Manifests) != 1 || !row.Manifests[0].Available || row.Manifests[0].Kind != "image" || row.Manifests[0].ImageData == nil {
			t.Fatalf("row %s missing API 1.47 Manifests: %+v", row.ID, row.Manifests)
		}
	}
	if got := byID[hostID].Manifests[0].ImageData.Platform.Architecture; got != "arm64" && got != hostImageArch {
		t.Errorf("host image platform arch = %q", got)
	}
	if got := byID[foreignOnlyID].Manifests[0].ImageData.Platform.Architecture; got != foreign {
		t.Errorf("foreign image platform arch = %q, want %q", got, foreign)
	}

	// Inspect by name tries host namespace first when both share a tag.
	st, body = te.do(t, http.MethodGet, "/images/app:1/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect app:1 = %d %s", st, body)
	}
	var ins imageInspectResponse
	_ = json.Unmarshal(body, &ins)
	if ins.ID != hostID {
		t.Fatalf("inspect app:1 resolved %s, want host %s", ins.ID, hostID)
	}

	// Inspect by foreign-only name resolves in foreign namespace.
	st, body = te.do(t, http.MethodGet, "/images/gvxd-amd64:1/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect gvxd-amd64:1 = %d %s", st, body)
	}
	ins = imageInspectResponse{}
	_ = json.Unmarshal(body, &ins)
	if ins.ID != foreignOnlyID || ins.Architecture != foreign {
		t.Fatalf("inspect gvxd-amd64:1 = (%s, %s), want (%s, %s)", ins.ID, ins.Architecture, foreignOnlyID, foreign)
	}

	// Inspect by foreign ID resolves the foreign image even when the tag also exists on host.
	st, body = te.do(t, http.MethodGet, "/images/"+foreignSameTagID+"/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect foreign ID = %d %s", st, body)
	}
	ins = imageInspectResponse{}
	_ = json.Unmarshal(body, &ins)
	if ins.ID != foreignSameTagID || ins.Architecture != foreign {
		t.Fatalf("inspect foreign ID = (%s, %s), want (%s, %s)", ins.ID, ins.Architecture, foreignSameTagID, foreign)
	}

	// Tag a foreign image stays in foreign namespace.
	st, body = te.do(t, http.MethodPost, "/images/gvxd-amd64:1/tag?repo=my/amd64&tag=v2", nil)
	if st != http.StatusCreated {
		t.Fatalf("tag foreign = %d %s", st, body)
	}
	st, body = te.do(t, http.MethodGet, "/images/my/amd64:v2/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect tagged foreign = %d %s", st, body)
	}

	// Save a foreign image routes through SaveImagesArch.
	st, body = te.do(t, http.MethodGet, "/images/gvxd-amd64:1/get", nil)
	if st != http.StatusOK || !strings.HasPrefix(string(body), foreign+":gvxd-amd64:1") {
		t.Fatalf("save foreign = %d %q", st, string(body))
	}

	// Delete the extra foreign tag leaves the image; delete by ID removes it.
	st, body = te.do(t, http.MethodDelete, "/images/my/amd64:v2", nil)
	if st != http.StatusOK || !strings.Contains(string(body), "Untagged") {
		t.Fatalf("untag foreign = %d %s", st, body)
	}
	st, body = te.do(t, http.MethodDelete, "/images/gvxd-amd64:1", nil)
	if st != http.StatusOK || !strings.Contains(string(body), "Deleted") {
		t.Fatalf("delete foreign = %d %s", st, body)
	}
	if st, _ := te.do(t, http.MethodGet, "/images/gvxd-amd64:1/json", nil); st != http.StatusNotFound {
		t.Fatalf("deleted foreign image still inspectable: %d", st)
	}

	// Prune -a removes unused foreign image too.
	q := url.QueryEscape(`{"dangling":["false"]}`)
	st, body = te.do(t, http.MethodPost, "/images/prune?filters="+q, nil)
	if st != http.StatusOK || !strings.Contains(string(body), foreignSameTagID) {
		t.Fatalf("prune -a = %d %s, expected %s deleted", st, body, foreignSameTagID)
	}
}

func TestForeignImageRunRefused(t *testing.T) {
	te := newTestEngine(t)
	foreign := foreignImageArch()
	te.fake.addForeignImage(foreign, "gvxd-amd64:1", ImageConfig{Cmd: []string{"/bin/sh"}})

	// Container create for a foreign-only image must return 400 (NOT 404) with foreignRunMessage.
	st, body := te.do(t, http.MethodPost, "/containers/create?name=try-amd64", map[string]any{
		"Image": "gvxd-amd64:1",
	})
	wantMsg := fmt.Sprintf(foreignRunMessage, foreign, "gvxd-amd64:1", foreign, hostImageArch)
	if st != http.StatusBadRequest {
		t.Fatalf("create foreign image = %d %s, want 400", st, body)
	}
	if !strings.Contains(string(body), wantMsg) {
		t.Fatalf("create foreign message = %s, want %q", body, wantMsg)
	}

	// Explicit foreign ?platform=linux/<foreign> is refused with 400 even if uncached.
	st, body = te.do(t, http.MethodPost, "/containers/create?name=try-plat&platform=linux/"+foreign, map[string]any{
		"Image": "alpine:3.20",
	})
	if st != http.StatusBadRequest || !strings.Contains(string(body), "not run, yet") {
		t.Fatalf("create foreign platform = %d %s, want 400 refusal", st, body)
	}

	// Missing image in both namespaces still returns 404 so docker CLI pulls.
	st, _ = te.do(t, http.MethodPost, "/containers/create?name=try-miss", map[string]any{
		"Image": "not-cached:1",
	})
	if st != http.StatusNotFound {
		t.Fatalf("create missing image = %d, want 404", st)
	}

	// Same tag cached in BOTH host and foreign -> runs host image cleanly.
	te.fake.addImage("gvxd-amd64:1", ImageConfig{Cmd: []string{"/bin/sh"}})
	st, body = te.do(t, http.MethodPost, "/containers/create?name=try-both", map[string]any{
		"Image": "gvxd-amd64:1",
	})
	if st != http.StatusCreated {
		t.Fatalf("create image present in both namespaces = %d %s, want 201", st, body)
	}
}

func TestForeignImagesLoadArchDetection(t *testing.T) {
	foreign := foreignImageArch()

	t.Run("docker_archive_plain_and_gzip", func(t *testing.T) {
		for _, gz := range []bool{false, true} {
			te := newTestEngine(t)
			cfgHash := strings.Repeat("a", 64)
			cfgJSON := fmt.Sprintf(`{"architecture":%q,"os":"linux"}`, foreign)
			manifestJSON := fmt.Sprintf(`[{"Config":"%s.json","RepoTags":["gvxd-amd64:1"]}]`, cfgHash)
			archive := buildTarArchive(t, gz, map[string][]byte{
				"manifest.json":   []byte(manifestJSON),
				cfgHash + ".json": []byte(cfgJSON),
			})

			st, body := te.do(t, http.MethodPost, "/images/load", archive)
			if st != http.StatusOK {
				t.Fatalf("load (gz=%v) = %d %s", gz, st, body)
			}
			got := te.fake.recordedArchLoads()
			if !slices.Equal(got, []string{foreign}) {
				t.Fatalf("load (gz=%v) called LoadImagesArch with %v, want [%s]", gz, got, foreign)
			}
		}
	})

	t.Run("oci_multi_arch_and_skip_attestation", func(t *testing.T) {
		te := newTestEngine(t)
		amdCfg := strings.Repeat("1", 64)
		armCfg := strings.Repeat("2", 64)
		amdMf := strings.Repeat("3", 64)
		armMf := strings.Repeat("4", 64)
		attMf := strings.Repeat("5", 64)

		indexJSON := fmt.Sprintf(`{
			"schemaVersion": 2,
			"manifests": [
				{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s"},
				{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s"},
				{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","platform":{"architecture":"unknown","os":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}}
			]
		}`, amdMf, armMf, attMf)
		amdMfJSON := fmt.Sprintf(`{"config":{"digest":"sha256:%s"}}`, amdCfg)
		armMfJSON := fmt.Sprintf(`{"config":{"digest":"sha256:%s"}}`, armCfg)

		archive := buildTarArchive(t, false, map[string][]byte{
			"index.json":             []byte(indexJSON),
			"blobs/sha256/" + amdMf:  []byte(amdMfJSON),
			"blobs/sha256/" + armMf:  []byte(armMfJSON),
			"blobs/sha256/" + amdCfg: []byte(`{"architecture":"amd64","os":"linux"}`),
			"blobs/sha256/" + armCfg: []byte(`{"architecture":"arm64","os":"linux"}`),
		})

		arches := detectArchiveArches(bytes.NewReader(archive))
		if !slices.Equal(arches, []string{"amd64", "arm64"}) {
			t.Fatalf("detectArchiveArches = %v, want [amd64 arm64]", arches)
		}

		st, body := te.do(t, http.MethodPost, "/images/load", archive)
		if st != http.StatusOK {
			t.Fatalf("load multi-arch OCI = %d %s", st, body)
		}
		got := te.fake.recordedArchLoads()
		if !slices.Equal(got, []string{foreign}) {
			t.Fatalf("foreign arch loads = %v, want [%s]", got, foreign)
		}
	})
}

// Regression test for review item 2: when the same repo:tag exists in both
// host and foreign namespaces, ?platform=linux/<foreign> scopes inspect, tag,
// save, and delete strictly to the foreign namespace without touching host.
func TestForeignImagesPlatformScopedCollision(t *testing.T) {
	te := newTestEngine(t)
	foreign := foreignImageArch()

	hostID := te.fake.addImage("shared:1", ImageConfig{Cmd: []string{"/host"}})
	foreignID := te.fake.addForeignImage(foreign, "shared:1", ImageConfig{Cmd: []string{"/foreign"}})

	// Inspect with ?platform=linux/<foreign> resolves the foreign image.
	st, body := te.do(t, http.MethodGet, "/images/shared:1/json?platform=linux/"+foreign, nil)
	if st != http.StatusOK {
		t.Fatalf("inspect foreign platform = %d %s", st, body)
	}
	var ins imageInspectResponse
	_ = json.Unmarshal(body, &ins)
	if ins.ID != foreignID || ins.Architecture != foreign {
		t.Fatalf("inspect foreign platform = (%s, %s), want (%s, %s)", ins.ID, ins.Architecture, foreignID, foreign)
	}

	// Tag with ?platform=linux/<foreign> tags ONLY the foreign image.
	st, body = te.do(t, http.MethodPost, "/images/shared:1/tag?repo=foreign-copy&tag=v1&platform=linux/"+foreign, nil)
	if st != http.StatusCreated {
		t.Fatalf("tag foreign platform = %d %s", st, body)
	}
	st, body = te.do(t, http.MethodGet, "/images/foreign-copy:v1/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect foreign-copy:v1 = %d %s", st, body)
	}
	ins = imageInspectResponse{}
	_ = json.Unmarshal(body, &ins)
	if ins.ID != foreignID {
		t.Fatalf("foreign-copy:v1 resolved %s, want foreign %s", ins.ID, foreignID)
	}

	// Save with ?platform=linux/<foreign> saves the foreign image.
	st, body = te.do(t, http.MethodGet, "/images/shared:1/get?platform=linux/"+foreign, nil)
	if st != http.StatusOK || !strings.HasPrefix(string(body), foreign+":shared:1") {
		t.Fatalf("save foreign platform = %d %q", st, string(body))
	}

	// Delete shared:1 with ?platform=linux/<foreign> removes ONLY the foreign tag,
	// leaving host shared:1 completely intact.
	st, body = te.do(t, http.MethodDelete, "/images/shared:1?platform=linux/"+foreign, nil)
	if st != http.StatusOK || !strings.Contains(string(body), "Untagged") {
		t.Fatalf("delete foreign platform = %d %s", st, body)
	}
	// Host shared:1 is still present.
	st, body = te.do(t, http.MethodGet, "/images/shared:1/json?platform=linux/"+hostImageArch, nil)
	if st != http.StatusOK {
		t.Fatalf("host shared:1 missing after deleting foreign shared:1: %d %s", st, body)
	}
	ins = imageInspectResponse{}
	_ = json.Unmarshal(body, &ins)
	if ins.ID != hostID {
		t.Fatalf("host shared:1 ID = %s, want %s", ins.ID, hostID)
	}
	// Foreign shared:1 is gone (404 when queried with foreign platform).
	if st, _ := te.do(t, http.MethodGet, "/images/shared:1/json?platform=linux/"+foreign, nil); st != http.StatusNotFound {
		t.Fatalf("foreign shared:1 still found after delete: %d", st)
	}

	// Invalid platform returns 400.
	if st, _ := te.do(t, http.MethodDelete, "/images/shared:1?platform=-bad", nil); st != http.StatusBadRequest {
		t.Fatalf("invalid platform delete = %d, want 400", st)
	}
}

// Regression test for review item 5a: POST /images/load enforces a spool size
// cap and cleans up temporary files on error.
func TestImageLoadSizeCapAndCleanup(t *testing.T) {
	oldMax := maxImageLoadBytes
	maxImageLoadBytes = 1024
	t.Cleanup(func() { maxImageLoadBytes = oldMax })

	te := newTestEngine(t)
	oversized := bytes.Repeat([]byte("a"), 2048)
	st, body := te.do(t, http.MethodPost, "/images/load", oversized)
	if st != http.StatusBadRequest || !strings.Contains(string(body), "exceeds size limit") {
		t.Fatalf("oversized load = %d %s, want 400 size limit error", st, body)
	}
	entries, err := os.ReadDir(filepath.Join(te.dir, "tmp"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized load left %d temp file(s) in tmp/", len(entries))
	}
}

// Regression test for review item 5b: GET /images/json avoids calling
// ImagesArch on every poll when amd64 builds are off and no foreign images
// exist, and resumes querying once a foreign image is loaded or amd64 turns on.
func TestImageListSkipsForeignWhenNoneAndAmd64Off(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})

	for range 3 {
		if st, body := te.do(t, http.MethodGet, "/images/json", nil); st != http.StatusOK {
			t.Fatalf("GET /images/json = %d %s", st, body)
		}
	}
	if got := te.fake.foreignImagesCalls(); got != 1 {
		t.Fatalf("foreign ImagesArch calls for 3 polls with no foreign images = %d, want 1", got)
	}

	// Loading a foreign archive wakes foreign listing immediately.
	foreign := foreignImageArch()
	cfgHash := strings.Repeat("c", 64)
	archive := buildTarArchive(t, false, map[string][]byte{
		"manifest.json":   []byte(fmt.Sprintf(`[{"Config":"%s.json"}]`, cfgHash)),
		cfgHash + ".json": []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux"}`, foreign)),
	})
	if st, body := te.do(t, http.MethodPost, "/images/load", archive); st != http.StatusOK {
		t.Fatalf("load foreign = %d %s", st, body)
	}
	st, body := te.do(t, http.MethodGet, "/images/json", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /images/json after foreign load = %d %s", st, body)
	}
	if got := te.fake.foreignImagesCalls(); got != 2 {
		t.Fatalf("foreign ImagesArch calls after foreign load = %d, want 2", got)
	}
	if !strings.Contains(string(body), "loaded-"+foreign+":1") {
		t.Fatalf("loaded foreign image missing from /images/json: %s", body)
	}
}
