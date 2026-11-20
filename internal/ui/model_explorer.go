package ui

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// The tabs of the image explorer dialog.
const (
	ExploreFiles = iota
	ExploreLayers
	ExploreInspect
)

const (
	previewMaxBytes = 256 * 1024 // 256 KiB text preview cap
	hexPreviewBytes = 4096       // 4 KiB hex dump cap for binary files
)

// FilePreview is the state of the preview pane in the image explorer.
type FilePreview struct {
	Entry     imagefs.Entry
	HasEntry  bool
	Loading   bool
	Err       string
	Body      string
	Language  string
	TotalSize int64
	Truncated bool
	Binary    bool
}

// Equal makes FilePreview comparable for signals.
func (p FilePreview) Equal(o FilePreview) bool {
	return p.HasEntry == o.HasEntry && p.Loading == o.Loading && p.Err == o.Err &&
		p.Body == o.Body && p.Language == o.Language && p.TotalSize == o.TotalSize &&
		p.Truncated == o.Truncated && p.Binary == o.Binary && p.Entry.Equal(o.Entry)
}

type explorerSession struct {
	mu         sync.Mutex
	fs         imagefs.FS
	gen        int
	cancel     context.CancelFunc
	fileGen    int
	fileCancel context.CancelFunc
}

func (s *explorerSession) reset() (gen int, ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileCancel != nil {
		s.fileCancel()
		s.fileCancel = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.fs != nil {
		_ = s.fs.Close()
		s.fs = nil
	}
	s.gen++
	s.fileGen++
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	return s.gen, ctx
}

func (s *explorerSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileCancel != nil {
		s.fileCancel()
		s.fileCancel = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.fs != nil {
		_ = s.fs.Close()
		s.fs = nil
	}
	s.gen++
	s.fileGen++
}

func (s *explorerSession) setFS(gen int, fs imagefs.FS) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen {
		if fs != nil {
			_ = fs.Close()
		}
		return false
	}
	s.fs = fs
	return true
}

func (s *explorerSession) getFS() (int, imagefs.FS) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen, s.fs
}

func (s *explorerSession) nextFileCtx() (gen, fileGen int, ctx context.Context, fs imagefs.FS) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileCancel != nil {
		s.fileCancel()
	}
	s.fileGen++
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	s.fileCancel = cancel
	return s.gen, s.fileGen, ctx, s.fs
}

// CloseExplorer cancels any in-flight explorer work and releases open layer
// files.
func (m *Model) CloseExplorer() {
	m.explorer.close()
	if ggui.Peek(m.ExploreOpen) {
		m.ExploreOpen.Set(false)
	}
}

func (m *Model) beginExplore(title, source string, tab int) (int, context.Context) {
	gen, ctx := m.explorer.reset()
	m.ExploreTitle.Set(title)
	m.ExploreSource.Set(source)
	m.ExploreTab.Set(tab)
	m.ExploreLoading.Set(true)
	m.ExploreProgress.Set("")
	m.ExploreErr.Set("")
	m.ExploreLayers.Set(nil)
	m.ExploreLayer.Set(0)
	m.ExplorePath.Set("/")
	m.ExploreQuery.Set("")
	m.ExploreEntries.Set(nil)
	m.ExploreSelected.Set("")
	m.ExplorePreview.Set(FilePreview{})
	m.ExploreInspect.Set(tr("Loading…"))
	m.ExploreOpen.Set(true)
	return gen, ctx
}

// ExploreGVXImage opens the image explorer for a cached gvx sandbox image on tab.
func (m *Model) ExploreGVXImage(img rungv.Image, tab int) {
	ref := img.Ref()
	gen, ctx := m.beginExplore(ref, "GVX", tab)
	if m.Offline {
		m.ExploreLoading.Set(false)
		return
	}
	cacheDir := ggui.Peek(m.Engine).Info.LayerCacheDir
	if cacheDir == "" {
		cacheDir = rungv.ExpandHome(ggui.Peek(m.Settings).CacheDir)
	}
	c := m.Client()

	// Load inspect JSON in parallel.
	go func() {
		ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		body, err := c.InspectImage(ictx, ref)
		if err != nil {
			body = err.Error()
		}
		m.post(func() {
			curGen, _ := m.explorer.getFS()
			if curGen == gen {
				m.ExploreInspect.Set(body)
			}
		})
	}()

	// Open EROFS layers and read root directory.
	go func() {
		fs, err := imagefs.OpenGVX(cacheDir, img.ID, img.Digest)
		if err != nil {
			m.post(func() {
				curGen, _ := m.explorer.getFS()
				if curGen == gen {
					m.ExploreLoading.Set(false)
					m.ExploreErr.Set(err.Error())
				}
			})
			return
		}
		if !m.explorer.setFS(gen, fs) {
			return
		}
		entries, derr := fs.ReadDir(ctx, "/", 0)
		layers := fs.Layers()
		m.post(func() {
			curGen, _ := m.explorer.getFS()
			if curGen != gen {
				return
			}
			m.ExploreLoading.Set(false)
			if derr != nil {
				m.ExploreErr.Set(derr.Error())
				return
			}
			m.ExploreLayers.Set(layers)
			m.ExploreEntries.Set(entries)
		})
	}()
}

// ExploreDockerImage opens the image explorer for a Docker Engine image on tab.
func (m *Model) ExploreDockerImage(img DockerImageRow, tab int) {
	ref := img.Ref()
	gen, ctx := m.beginExplore(ref, "Docker Engine", tab)
	if m.Offline {
		m.ExploreLoading.Set(false)
		return
	}
	host := m.dockerBackendHost()

	// Load inspect JSON in parallel.
	go func() {
		c, err := dockerapi.New(host)
		var body string
		if err != nil {
			body = err.Error()
		} else {
			defer c.Close()
			ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			body, err = c.InspectImage(ictx, img.ID)
			if err != nil {
				body = err.Error()
			}
		}
		m.post(func() {
			curGen, _ := m.explorer.getFS()
			if curGen == gen {
				m.ExploreInspect.Set(body)
			}
		})
	}()

	// Index Docker image filesystem and layer history.
	go func() {
		var last time.Time
		fs, err := imagefs.OpenDocker(ctx, host, img.ID, func(n int) {
			if now := time.Now(); now.Sub(last) > 120*time.Millisecond {
				last = now
				msg := tr("Indexing image… %d entries", n)
				m.post(func() {
					curGen, _ := m.explorer.getFS()
					if curGen == gen {
						m.ExploreProgress.Set(msg)
					}
				})
			}
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.post(func() {
				curGen, _ := m.explorer.getFS()
				if curGen == gen {
					m.ExploreLoading.Set(false)
					m.ExploreErr.Set(err.Error())
				}
			})
			return
		}
		if !m.explorer.setFS(gen, fs) {
			return
		}
		entries, derr := fs.ReadDir(ctx, "/", 0)
		layers := fs.Layers()
		m.post(func() {
			curGen, _ := m.explorer.getFS()
			if curGen != gen {
				return
			}
			m.ExploreLoading.Set(false)
			m.ExploreProgress.Set("")
			if derr != nil {
				m.ExploreErr.Set(derr.Error())
				return
			}
			m.ExploreLayers.Set(layers)
			m.ExploreEntries.Set(entries)
		})
	}()
}

// ExploreOpenDir navigates to dir in the active image filesystem.
func (m *Model) ExploreOpenDir(dir string) {
	dir = imagefs.CleanPath(dir)
	gen, fs := m.explorer.getFS()
	if fs == nil {
		return
	}
	layer := ggui.Peek(m.ExploreLayer)
	m.ExplorePath.Set(dir)
	m.ExploreQuery.Set("")
	m.ExploreSelected.Set("")
	m.ExplorePreview.Set(FilePreview{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		entries, err := fs.ReadDir(ctx, dir, layer)
		m.post(func() {
			curGen, _ := m.explorer.getFS()
			if curGen != gen || ggui.Peek(m.ExplorePath) != dir || ggui.Peek(m.ExploreLayer) != layer {
				return
			}
			if err != nil {
				m.ExploreErr.Set(err.Error())
				return
			}
			m.ExploreErr.Set("")
			m.ExploreEntries.Set(entries)
		})
	}()
}

// ExploreUp moves to the parent directory.
func (m *Model) ExploreUp() {
	cur := ggui.Peek(m.ExplorePath)
	if cur == "" || cur == "/" {
		return
	}
	m.ExploreOpenDir(path.Dir(cur))
}

// ExploreSetLayer filters the Files view to layer (0 for merged all-layers)
// and reloads the current directory.
func (m *Model) ExploreSetLayer(layer int) {
	m.ExploreLayer.Set(layer)
	m.ExploreOpenDir(ggui.Peek(m.ExplorePath))
}

// ExploreBrowseLayer jumps from the Layers tab to the Files tab filtered to layer.
func (m *Model) ExploreBrowseLayer(layer int) {
	m.ExploreLayer.Set(layer)
	m.ExploreTab.Set(ExploreFiles)
	m.ExploreOpenDir("/")
}

// ExploreSelectEntry opens a directory or previews a file/symlink.
func (m *Model) ExploreSelectEntry(e imagefs.Entry) {
	if e.Kind == imagefs.KindDir {
		m.ExploreOpenDir(e.Path)
		return
	}
	m.ExploreSelected.Set(e.Path)
	if e.Kind != imagefs.KindFile {
		m.ExplorePreview.Set(FilePreview{HasEntry: true, Entry: e})
		return
	}
	gen, fileGen, ctx, fs := m.explorer.nextFileCtx()
	if fs == nil {
		return
	}
	layer := ggui.Peek(m.ExploreLayer)
	m.ExplorePreview.Set(FilePreview{HasEntry: true, Entry: e, Loading: true, TotalSize: e.Size})
	go func() {
		fc, err := fs.ReadFile(ctx, e.Path, layer, previewMaxBytes)
		var prev FilePreview
		prev.HasEntry = true
		prev.Entry = e
		if err != nil {
			prev.Err = err.Error()
		} else {
			prev.TotalSize = fc.TotalSize
			prev.Binary = fc.Binary
			prev.Truncated = fc.Truncated
			if fc.Binary {
				prev.Body = imagefs.HexDump(fc.Data, hexPreviewBytes)
				prev.Language = "hex"
				if int64(len(fc.Data)) > hexPreviewBytes || fc.TotalSize > hexPreviewBytes {
					prev.Truncated = true
				}
			} else {
				prev.Body = string(fc.Data)
				prev.Language = imagefs.DetectLanguage(e.Name)
			}
		}
		m.post(func() {
			m.explorer.mu.Lock()
			ok := m.explorer.gen == gen && m.explorer.fileGen == fileGen
			m.explorer.mu.Unlock()
			if ok {
				m.ExplorePreview.Set(prev)
			}
		})
	}()
}

// ExploreFollowSymlink navigates to the resolved target of a symlink entry.
func (m *Model) ExploreFollowSymlink(e imagefs.Entry) {
	if e.Kind != imagefs.KindSymlink || e.Target == "" {
		return
	}
	target := imagefs.ResolveSymlink(e.Path, e.Target)
	gen, fs := m.explorer.getFS()
	if fs == nil {
		return
	}
	layer := ggui.Peek(m.ExploreLayer)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// Look up target in its parent directory to know whether it is a dir or file.
		parent := path.Dir(target)
		base := path.Base(target)
		if target == "/" {
			m.post(func() { m.ExploreOpenDir("/") })
			return
		}
		siblings, err := fs.ReadDir(ctx, parent, layer)
		if err != nil {
			m.post(func() { m.toastError(tr("Could not open %s", target), err) })
			return
		}
		var found *imagefs.Entry
		for i := range siblings {
			if siblings[i].Name == base {
				found = &siblings[i]
				break
			}
		}
		m.post(func() {
			curGen, _ := m.explorer.getFS()
			if curGen != gen {
				return
			}
			if found == nil {
				m.toastError(tr("Could not open %s", target), fmt.Errorf("%s", tr("Target not found in image.")))
				return
			}
			if found.Kind == imagefs.KindDir {
				m.ExploreOpenDir(found.Path)
				return
			}
			m.ExplorePath.Set(parent)
			m.ExploreQuery.Set("")
			m.ExploreEntries.Set(siblings)
			m.ExploreSelectEntry(*found)
		})
	}()
}

// filterExploreEntries filters entries by substring match on name.
func filterExploreEntries(entries []imagefs.Entry, q string) []imagefs.Entry {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return entries
	}
	var out []imagefs.Entry
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name), q) {
			out = append(out, e)
		}
	}
	return out
}
