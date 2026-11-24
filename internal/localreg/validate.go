package localreg

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	maxRepoNameLen = 255
	maxTagLen      = 128
)

var (
	repoNameRe = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	tagRe      = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	uuidRe     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func validRepoName(name string) bool {
	return len(name) > 0 && len(name) <= maxRepoNameLen && repoNameRe.MatchString(name)
}

func validTag(tag string) bool {
	return len(tag) > 0 && len(tag) <= maxTagLen && tagRe.MatchString(tag)
}

func validUUID(id string) bool {
	return len(id) == 36 && uuidRe.MatchString(id)
}

// parseDigest validates a sha256 digest of the form "sha256:<64 lowercase hex>"
// and returns the 64-character hex portion.
func parseDigest(d string) (string, bool) {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hexPart) != 64 {
		return "", false
	}
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", false
		}
	}
	return hexPart, true
}

func sha256HexString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

type routeKind int

const (
	routeUnknown routeKind = iota
	routeBase
	routeCatalog
	routeTagsList
	routeBlob
	routeUploadsInit
	routeUpload
	routeManifest
)

type v2Route struct {
	kind      routeKind
	repo      string
	digest    string // full "sha256:<hex>"
	digestHex string // 64-char hex
	tag       string
	isDigest  bool
	uuid      string
}

func hasUnsafeRawPath(rawPath string) bool {
	if rawPath == "" {
		return false
	}
	lower := strings.ToLower(rawPath)
	return strings.Contains(lower, "%2f") ||
		strings.Contains(lower, "%5c") ||
		strings.Contains(lower, "%2e") ||
		strings.Contains(lower, "%00")
}

// parseV2Path parses an incoming HTTP request path under /v2/ without relying
// on single-segment ServeMux wildcards, supporting arbitrary-depth repository
// names (including segments such as "tags", "blobs", "manifests", "uploads").
func parseV2Path(rawPath, urlPath string) (v2Route, int, string, string) {
	if hasUnsafeRawPath(rawPath) || strings.ContainsAny(urlPath, "\\\x00") {
		return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid path"
	}
	if urlPath == "/v2" || urlPath == "/v2/" {
		return v2Route{kind: routeBase}, 0, "", ""
	}
	if !strings.HasPrefix(urlPath, "/v2/") {
		return v2Route{}, http.StatusNotFound, "UNSUPPORTED", "not found"
	}

	rest := strings.TrimPrefix(urlPath, "/v2/")
	if rest == "_catalog" {
		return v2Route{kind: routeCatalog}, 0, "", ""
	}

	// Handle trailing slash (only valid for /v2/<repo>/blobs/uploads/).
	if strings.HasSuffix(rest, "/") {
		trimmed := strings.TrimSuffix(rest, "/")
		for _, seg := range strings.Split(trimmed, "/") {
			if seg == "" || seg == "." || seg == ".." {
				return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid path segment"
			}
		}
		if rest == "blobs/uploads/" {
			return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid repository name"
		}
		if strings.HasSuffix(rest, "/blobs/uploads/") {
			repo := strings.TrimSuffix(rest, "/blobs/uploads/")
			if !validRepoName(repo) {
				return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid repository name"
			}
			return v2Route{kind: routeUploadsInit, repo: repo}, 0, "", ""
		}
		return v2Route{}, http.StatusNotFound, "UNSUPPORTED", "not found"
	}

	segs := strings.Split(rest, "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid path segment"
		}
	}

	n := len(segs)

	// /v2/<name>/tags/list
	if n >= 3 && segs[n-2] == "tags" && segs[n-1] == "list" {
		repo := strings.Join(segs[:n-2], "/")
		if !validRepoName(repo) {
			return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid repository name"
		}
		return v2Route{kind: routeTagsList, repo: repo}, 0, "", ""
	}

	// /v2/<name>/blobs/uploads (without trailing slash)
	if n >= 3 && segs[n-2] == "blobs" && segs[n-1] == "uploads" {
		repo := strings.Join(segs[:n-2], "/")
		if !validRepoName(repo) {
			return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid repository name"
		}
		return v2Route{kind: routeUploadsInit, repo: repo}, 0, "", ""
	}

	// /v2/<name>/blobs/uploads/<uuid>
	if n >= 4 && segs[n-3] == "blobs" && segs[n-2] == "uploads" {
		repo := strings.Join(segs[:n-3], "/")
		if !validRepoName(repo) {
			return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid repository name"
		}
		uuid := segs[n-1]
		if !validUUID(uuid) {
			return v2Route{}, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "invalid upload uuid"
		}
		return v2Route{kind: routeUpload, repo: repo, uuid: uuid}, 0, "", ""
	}

	// /v2/<name>/blobs/<digest>
	if n >= 3 && segs[n-2] == "blobs" {
		repo := strings.Join(segs[:n-2], "/")
		if !validRepoName(repo) {
			return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid repository name"
		}
		digest := segs[n-1]
		digestHex, ok := parseDigest(digest)
		if !ok {
			return v2Route{}, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest"
		}
		return v2Route{kind: routeBlob, repo: repo, digest: digest, digestHex: digestHex}, 0, "", ""
	}

	// /v2/<name>/manifests/<reference>
	if n >= 3 && segs[n-2] == "manifests" {
		repo := strings.Join(segs[:n-2], "/")
		if !validRepoName(repo) {
			return v2Route{}, http.StatusBadRequest, "NAME_INVALID", "invalid repository name"
		}
		ref := segs[n-1]
		if strings.Contains(ref, ":") {
			digestHex, ok := parseDigest(ref)
			if !ok {
				return v2Route{}, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest"
			}
			return v2Route{
				kind:      routeManifest,
				repo:      repo,
				digest:    ref,
				digestHex: digestHex,
				isDigest:  true,
			}, 0, "", ""
		}
		if !validTag(ref) {
			return v2Route{}, http.StatusBadRequest, "TAG_INVALID", "invalid tag"
		}
		return v2Route{
			kind:     routeManifest,
			repo:     repo,
			tag:      ref,
			isDigest: false,
		}, 0, "", ""
	}

	return v2Route{}, http.StatusNotFound, "UNSUPPORTED", "not found"
}
