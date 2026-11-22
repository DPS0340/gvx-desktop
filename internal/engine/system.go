package engine

// Owner: workstream C.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (e *Engine) ping(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Api-Version", APIVersion)
	h.Set("Ostype", "linux")
	h.Set("Docker-Experimental", "false")
	h.Set("Builder-Version", "1")
	h.Set("Swarm", "inactive")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("OK"))
	}
}

func (e *Engine) serverVersion() string {
	v := strings.TrimSpace(e.cfg.Version)
	if v == "" {
		v = "dev"
	}
	return "gvx-desktop-" + v
}

func (e *Engine) version(w http.ResponseWriter, r *http.Request) {
	ver := e.serverVersion()
	rungvVer := ""
	if rv, ok := e.runner.(interface {
		RungvVersion(context.Context) (string, error)
	}); ok {
		if v, err := rv.RungvVersion(r.Context()); err == nil {
			rungvVer = v
		}
	}

	type component struct {
		Name    string            `json:"Name"`
		Version string            `json:"Version"`
		Details map[string]string `json:"Details,omitempty"`
	}
	resp := struct {
		Platform      struct{ Name string } `json:"Platform"`
		Components    []component           `json:"Components"`
		Version       string                `json:"Version"`
		APIVersion    string                `json:"ApiVersion"`
		MinAPIVersion string                `json:"MinAPIVersion"`
		Os            string                `json:"Os"`
		Arch          string                `json:"Arch"`
		KernelVersion string                `json:"KernelVersion"`
		GoVersion     string                `json:"GoVersion"`
		BuildTime     string                `json:"BuildTime"`
		Experimental  bool                  `json:"Experimental"`
	}{
		Platform: struct{ Name string }{Name: "GVX Desktop sandbox engine"},
		Components: []component{
			{
				Name:    "Engine",
				Version: ver,
				Details: map[string]string{
					"ApiVersion":    APIVersion,
					"MinAPIVersion": MinAPIVersion,
					"Os":            "linux",
					"Arch":          runtime.GOARCH,
					"GoVersion":     runtime.Version(),
					"Experimental":  "true",
					"GitCommit":     "",
				},
			},
			{
				Name:    "rungv",
				Version: rungvVer,
			},
		},
		Version:       ver,
		APIVersion:    APIVersion,
		MinAPIVersion: MinAPIVersion,
		Os:            "linux",
		Arch:          runtime.GOARCH,
		KernelVersion: "gvx",
		GoVersion:     runtime.Version(),
		BuildTime:     "",
		Experimental:  true,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (e *Engine) info(w http.ResponseWriter, r *http.Request) {
	ctrs := e.allContainers()
	var running, paused, stopped int
	for _, c := range ctrs {
		if e.isContainerPaused(c) {
			paused++
		} else if c.State.Running() {
			running++
		} else {
			stopped++
		}
	}

	imgCount := 0
	if imgs, err := e.runner.Images(r.Context()); err == nil {
		seen := map[string]bool{}
		for _, img := range imgs {
			seen[img.ID] = true
		}
		imgCount = len(seen)
	}

	ncpu := runtime.NumCPU()
	if ri, err := e.runner.Info(r.Context()); err == nil && ri.NumCPU > 0 {
		ncpu = ri.NumCPU
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "gvx-desktop"
	}

	infoWarning := "GVX Desktop's experimental sandbox engine: exec and cp are not supported yet."
	if e.runner.Features().Control {
		infoWarning = "GVX Desktop's experimental sandbox engine: commit, export and diff are not supported yet."
	}

	resp := struct {
		ID                 string              `json:"ID"`
		Name               string              `json:"Name"`
		ServerVersion      string              `json:"ServerVersion"`
		Containers         int                 `json:"Containers"`
		ContainersRunning  int                 `json:"ContainersRunning"`
		ContainersPaused   int                 `json:"ContainersPaused"`
		ContainersStopped  int                 `json:"ContainersStopped"`
		Images             int                 `json:"Images"`
		Driver             string              `json:"Driver"`
		DockerRootDir      string              `json:"DockerRootDir"`
		OperatingSystem    string              `json:"OperatingSystem"`
		OSType             string              `json:"OSType"`
		Architecture       string              `json:"Architecture"`
		NCPU               int                 `json:"NCPU"`
		MemTotal           int64               `json:"MemTotal"`
		KernelVersion      string              `json:"KernelVersion"`
		IndexServerAddress string              `json:"IndexServerAddress"`
		RegistryConfig     map[string]any      `json:"RegistryConfig"`
		Swarm              map[string]any      `json:"Swarm"`
		Plugins            map[string][]string `json:"Plugins"`
		SecurityOptions    []string            `json:"SecurityOptions"`
		Runtimes           map[string]any      `json:"Runtimes"`
		DefaultRuntime     string              `json:"DefaultRuntime"`
		LoggingDriver      string              `json:"LoggingDriver"`
		CgroupDriver       string              `json:"CgroupDriver"`
		ExperimentalBuild  bool                `json:"ExperimentalBuild"`
		Warnings           []string            `json:"Warnings"`
	}{
		ID:                 engineUUID(e.cfg.Dir),
		Name:               hostname,
		ServerVersion:      e.serverVersion(),
		Containers:         len(ctrs),
		ContainersRunning:  running,
		ContainersPaused:   paused,
		ContainersStopped:  stopped,
		Images:             imgCount,
		Driver:             "gvx",
		DockerRootDir:      e.cfg.Dir,
		OperatingSystem:    "GVX Desktop sandbox engine",
		OSType:             "linux",
		Architecture:       dockerArch(runtime.GOARCH),
		NCPU:               ncpu,
		MemTotal:           hostMemTotal(),
		KernelVersion:      "gvx",
		IndexServerAddress: "https://index.docker.io/v1/",
		RegistryConfig:     map[string]any{},
		Swarm:              map[string]any{"LocalNodeState": "inactive"},
		Plugins: map[string][]string{
			"Volume":  {"local"},
			"Network": {"bridge", "host", "null"},
		},
		SecurityOptions:   []string{},
		Runtimes:          map[string]any{},
		DefaultRuntime:    "gvx",
		LoggingDriver:     "json-file",
		CgroupDriver:      "none",
		ExperimentalBuild: true,
		Warnings:          []string{infoWarning},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (e *Engine) systemDF(w http.ResponseWriter, r *http.Request) {
	imgs, err := e.buildImageSummaries(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	var layersSize int64
	imgSizes := make(map[string]int64, len(imgs))
	for _, img := range imgs {
		layersSize += img.Size
		imgSizes[img.ID] = img.Size
	}

	ctrs := e.allContainers()
	type dfContainer struct {
		ID              string            `json:"Id"`
		Names           []string          `json:"Names"`
		Image           string            `json:"Image"`
		ImageID         string            `json:"ImageID"`
		Command         string            `json:"Command"`
		Created         int64             `json:"Created"`
		State           string            `json:"State"`
		Status          string            `json:"Status"`
		SizeRw          int64             `json:"SizeRw"`
		SizeRootFs      int64             `json:"SizeRootFs"`
		Labels          map[string]string `json:"Labels"`
		HostConfig      map[string]string `json:"HostConfig"`
		NetworkSettings map[string]any    `json:"NetworkSettings"`
		Mounts          []MountPoint      `json:"Mounts"`
	}
	dfCtrs := make([]dfContainer, 0, len(ctrs))
	for _, c := range ctrs {
		labels := c.Config.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		mounts := c.Mounts
		if mounts == nil {
			mounts = []MountPoint{}
		}
		cmd := strings.Join(append(append([]string{}, c.Config.Entrypoint...), c.Config.Cmd...), " ")
		dfCtrs = append(dfCtrs, dfContainer{
			ID:              c.ID,
			Names:           []string{"/" + c.Name},
			Image:           c.Image,
			ImageID:         c.ImageID,
			Command:         cmd,
			Created:         c.Created.Unix(),
			State:           c.State.Status,
			Status:          c.State.Status,
			SizeRw:          0,
			SizeRootFs:      imgSizes[c.ImageID],
			Labels:          labels,
			HostConfig:      map[string]string{"NetworkMode": c.HostConfig.NetworkMode},
			NetworkSettings: map[string]any{"Networks": c.Networks},
			Mounts:          mounts,
		})
	}

	vols := e.snapshotVolumes()
	rvMap := e.rungvVolumeMap(r.Context())
	refs := e.volumeRefCounts()
	dfVols := make([]dockerVolume, 0, len(vols))
	for _, v := range vols {
		rv := rvMap[v.Name]
		usage := &volumeUsageData{
			Size:     parseHumanSize(rv.Size),
			RefCount: refs[v.Name],
		}
		dfVols = append(dfVols, toDockerVolume(v, rv, usage))
	}

	var imgActive, imgReclaim int64
	for _, img := range imgs {
		if img.Containers > 0 {
			imgActive++
		} else {
			imgReclaim += img.Size
		}
	}
	var ctrActive int64
	for _, c := range ctrs {
		if c.State.Running() {
			ctrActive++
		}
	}
	var volActive, volTotal, volReclaim int64
	for _, v := range dfVols {
		sz := int64(0)
		if v.UsageData != nil {
			sz = v.UsageData.Size
			if v.UsageData.RefCount > 0 {
				volActive++
			} else {
				volReclaim += sz
			}
		}
		volTotal += sz
	}

	type diskUsageSummary[T any] struct {
		ActiveCount int64 `json:"ActiveCount"`
		TotalCount  int64 `json:"TotalCount"`
		Reclaimable int64 `json:"Reclaimable"`
		TotalSize   int64 `json:"TotalSize"`
		Items       []T   `json:"Items"`
	}

	writeJSON(w, http.StatusOK, struct {
		LayersSize      int64                          `json:"LayersSize"`
		Images          []imageSummary                 `json:"Images"`
		Containers      []dfContainer                  `json:"Containers"`
		Volumes         []dockerVolume                 `json:"Volumes"`
		BuildCache      []any                          `json:"BuildCache"`
		ImageUsage      diskUsageSummary[imageSummary] `json:"ImageUsage"`
		ContainerUsage  diskUsageSummary[dfContainer]  `json:"ContainerUsage"`
		VolumeUsage     diskUsageSummary[dockerVolume] `json:"VolumeUsage"`
		BuildCacheUsage diskUsageSummary[any]          `json:"BuildCacheUsage"`
	}{
		LayersSize: layersSize,
		Images:     imgs,
		Containers: dfCtrs,
		Volumes:    dfVols,
		BuildCache: []any{},
		ImageUsage: diskUsageSummary[imageSummary]{
			ActiveCount: imgActive,
			TotalCount:  int64(len(imgs)),
			Reclaimable: imgReclaim,
			TotalSize:   layersSize,
			Items:       imgs,
		},
		ContainerUsage: diskUsageSummary[dfContainer]{
			ActiveCount: ctrActive,
			TotalCount:  int64(len(dfCtrs)),
			Reclaimable: 0,
			TotalSize:   0,
			Items:       dfCtrs,
		},
		VolumeUsage: diskUsageSummary[dockerVolume]{
			ActiveCount: volActive,
			TotalCount:  int64(len(dfVols)),
			Reclaimable: volReclaim,
			TotalSize:   volTotal,
			Items:       dfVols,
		},
		BuildCacheUsage: diskUsageSummary[any]{
			Items: []any{},
		},
	})
}

func engineUUID(dir string) string {
	sum := sha256.Sum256([]byte("gvx-desktop-engine:" + dir))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func dockerArch(goarch string) string {
	switch goarch {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return goarch
	}
}

func formatInt64(n int64) string { return strconv.FormatInt(n, 10) }

// parseHumanSize parses human-readable byte sizes such as "9.155MB", "1.5GiB",
// "512kB", or "0B".
func parseHumanSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Strip trailing parentheticals like "0B (0%)" if present.
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0
	}
	val, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	var mult float64
	switch unit {
	case "", "b":
		mult = 1
	case "kb", "k":
		mult = 1e3
	case "kib":
		mult = 1024
	case "mb", "m":
		mult = 1e6
	case "mib":
		mult = 1024 * 1024
	case "gb", "g":
		mult = 1e9
	case "gib":
		mult = 1024 * 1024 * 1024
	case "tb", "t":
		mult = 1e12
	case "tib":
		mult = 1024 * 1024 * 1024 * 1024
	default:
		mult = 1
	}
	return int64(val * mult)
}

func parseRungvTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 -0700 -0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("unrecognized time format")
}
