// Package localreg implements an embedded, disk-persistent, loopback-only
// OCI Distribution v1.1 / Docker Registry HTTP API V2 server for GVX Desktop.
package localreg

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// DefaultPort is the default loopback TCP port for the built-in registry.
const DefaultPort = 5001

const (
	apiVersionHeader          = "Docker-Distribution-Api-Version"
	apiVersionValue           = "registry/2.0"
	maxManifestSize           = 16 << 20 // 16 MiB
	maxConfigBlobSize         = 4 << 20  // 4 MiB safety cap when inspecting config JSON
	mediaTypeOCIImageManifest = "application/vnd.oci.image.manifest.v1+json"
)

var errNoDir = errors.New("localreg: storage directory is required")

// Config configures a local OCI registry Server.
type Config struct {
	Dir      string // persistent storage directory (e.g. <dataDir>/registry)
	Port     int    // 0 -> DefaultPort (5001); <0 -> ephemeral 127.0.0.1:0 (for tests)
	OnChange func() // optional callback invoked safely outside locks when manifests/tags/GC mutate storage
}

// ImageEntry describes one tagged image stored in the registry.
type ImageEntry struct {
	Repository string
	Tag        string
	Digest     string
	MediaType  string
	Size       int64    // total unique byte size of manifest + config + layers + child manifests referenced by this image
	Platforms  []string // sorted unique os/arch[/variant] strings
	UpdatedAt  time.Time
}

// Key returns "repository:tag".
func (e ImageEntry) Key() string {
	return e.Repository + ":" + e.Tag
}

// Ref returns "host/repository:tag", or Key() when host is empty.
func (e ImageEntry) Ref(host string) string {
	host = strings.TrimRight(host, "/")
	if host == "" {
		return e.Key()
	}
	return host + "/" + e.Key()
}

// ShortDigest returns the 12-character hex prefix after "sha256:", or a
// 12-character truncation of Digest for other formats.
func (e ImageEntry) ShortDigest() string {
	d := strings.TrimPrefix(e.Digest, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// Equal reports whether e and o describe the same image entry.
func (e ImageEntry) Equal(o ImageEntry) bool {
	return e.Repository == o.Repository &&
		e.Tag == o.Tag &&
		e.Digest == o.Digest &&
		e.MediaType == o.MediaType &&
		e.Size == o.Size &&
		e.UpdatedAt.Equal(o.UpdatedAt) &&
		slices.Equal(e.Platforms, o.Platforms)
}

// DiskStats summarizes disk usage and image/blob counts in the registry.
type DiskStats struct {
	TotalBytes       int64
	ReclaimableBytes int64
	ImageCount       int
	BlobCount        int
}

// GCResult summarizes objects removed by garbage collection.
type GCResult struct {
	RemovedBlobs     int
	RemovedManifests int
	ReclaimedBytes   int64
}

// Internal OCI manifest / descriptor / config JSON representations.

type ociPlatform struct {
	Architecture string `json:"architecture,omitempty"`
	OS           string `json:"os,omitempty"`
	Variant      string `json:"variant,omitempty"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *ociPlatform      `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion,omitempty"`
	MediaType     string          `json:"mediaType,omitempty"`
	Config        *ociDescriptor  `json:"config,omitempty"`
	Layers        []ociDescriptor `json:"layers,omitempty"`
	Manifests     []ociDescriptor `json:"manifests,omitempty"`
}

type ociImageConfig struct {
	OS           string `json:"os,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Variant      string `json:"variant,omitempty"`
}

// On-disk metadata JSON formats.

type repoMeta struct {
	Name string `json:"name"`
}

type manifestMeta struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type tagRecord struct {
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// OCI Distribution API error payload.

type ociError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ociErrorResponse struct {
	Errors []ociError `json:"errors"`
}
