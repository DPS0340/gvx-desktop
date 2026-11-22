package engine

// Owner: workstream C. Extends fakeRunner (defined in frozen fake_test.go)
// to implement ImageArchRunner with per-architecture image namespaces.

import (
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

var _ ImageArchRunner = (*fakeRunner)(nil)

type fakeArchStore struct {
	mu          sync.Mutex
	byArch      map[string]map[string]ImageInspect
	loads       []string // recorded LoadImagesArch arches
	imagesCalls int      // count of ImagesArch calls for foreign arch
}

var fakeArchStores sync.Map // *fakeRunner -> *fakeArchStore

func (f *fakeRunner) archStore() *fakeArchStore {
	if v, ok := fakeArchStores.Load(f); ok {
		return v.(*fakeArchStore)
	}
	s := &fakeArchStore{byArch: map[string]map[string]ImageInspect{}}
	actual, _ := fakeArchStores.LoadOrStore(f, s)
	return actual.(*fakeArchStore)
}

func (s *fakeArchStore) ns(arch string) map[string]ImageInspect {
	m := s.byArch[arch]
	if m == nil {
		m = map[string]ImageInspect{}
		s.byArch[arch] = m
	}
	return m
}

// addForeignImage caches an image under ref in arch's namespace and returns its ID.
func (f *fakeRunner) addForeignImage(arch, ref string, cfg ImageConfig) string {
	if arch == "" || arch == hostImageArch {
		return f.addImage(ref, cfg)
	}
	s := f.archStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	id := "sha256:" + fakeHex(arch+":"+ref)
	img := ImageInspect{
		ID:           id,
		RepoTags:     []string{ref},
		Created:      time.Unix(1700000100, 0).UTC(),
		Architecture: arch,
		Os:           "linux",
		Config:       cfg,
		Size:         2 << 20,
	}
	ns := s.ns(arch)
	ns[ref] = img
	ns[id] = img
	return id
}

func (f *fakeRunner) recordedArchLoads() []string {
	s := f.archStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.loads)
}

func (f *fakeRunner) foreignImagesCalls() int {
	s := f.archStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.imagesCalls
}

func (f *fakeRunner) ImagesArch(ctx context.Context, arch string) ([]rungv.Image, error) {
	if arch == "" || arch == hostImageArch {
		return f.Images(ctx)
	}
	s := f.archStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imagesCalls++
	ns := s.byArch[arch]
	var out []rungv.Image
	for _, k := range slices.Sorted(maps.Keys(ns)) {
		img := ns[k]
		if k == img.ID {
			continue
		}
		repo, tag, _ := strings.Cut(k, ":")
		out = append(out, rungv.Image{
			ID:         img.ID,
			Repository: repo,
			Tag:        tag,
			Size:       "2.097MB",
			CreatedAt:  img.Created.Format("2006-01-02 15:04:05 -0700 MST"),
		})
	}
	return out, nil
}

func (f *fakeRunner) InspectImagesArch(ctx context.Context, arch string, refs ...string) ([]ImageInspect, error) {
	if arch == "" || arch == hostImageArch {
		return f.InspectImages(ctx, refs...)
	}
	s := f.archStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	ns := s.byArch[arch]
	var out []ImageInspect
	for _, ref := range refs {
		img, ok := ns[ref]
		if !ok {
			img, ok = ns[ref+":latest"]
		}
		if !ok {
			return nil, &rungv.Error{Args: []string{"inspect", "-arch", arch, ref}, Code: 1, Stderr: "rungv inspect: image " + ref + " not found in the cache"}
		}
		out = append(out, img)
	}
	return out, nil
}

func (f *fakeRunner) RemoveImageArch(ctx context.Context, arch, ref string, force bool) error {
	if arch == "" || arch == hostImageArch {
		return f.RemoveImage(ctx, ref, force)
	}
	s := f.archStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	ns := s.byArch[arch]
	img, ok := ns[ref]
	if !ok {
		return &rungv.Error{Args: []string{"rmi", "-arch", arch, ref}, Code: 1, Stderr: "rungv rmi: image " + ref + " not found"}
	}
	delete(ns, ref)
	if ref == img.ID {
		for k, v := range ns {
			if v.ID == img.ID {
				delete(ns, k)
			}
		}
	} else {
		stillTagged := false
		for k, v := range ns {
			if k != img.ID && v.ID == img.ID {
				stillTagged = true
				break
			}
		}
		if !stillTagged {
			delete(ns, img.ID)
		}
	}
	return nil
}

func (f *fakeRunner) TagImageArch(ctx context.Context, arch, source, target string) error {
	if arch == "" || arch == hostImageArch {
		return f.TagImage(ctx, source, target)
	}
	s := f.archStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	ns := s.ns(arch)
	img, ok := ns[source]
	if !ok {
		return &rungv.Error{Args: []string{"tag", "-arch", arch, source, target}, Code: 1, Stderr: "rungv tag: image " + source + " not found"}
	}
	img.RepoTags = append(slices.Clone(img.RepoTags), target)
	ns[target] = img
	ns[img.ID] = img
	return nil
}

func (f *fakeRunner) LoadImagesArch(ctx context.Context, arch string, archive io.Reader, line func(string)) error {
	if arch == "" || arch == hostImageArch {
		return f.LoadImages(ctx, archive, line)
	}
	b, err := io.ReadAll(archive)
	if err != nil {
		return err
	}
	s := f.archStore()
	s.mu.Lock()
	s.loads = append(s.loads, arch)
	s.mu.Unlock()

	ref := "loaded-" + arch + ":1"
	if txt := strings.TrimSpace(string(b)); txt != "" && !strings.ContainsAny(txt, "\x00\n") && len(txt) < 120 {
		ref = txt
	}
	f.addForeignImage(arch, ref, ImageConfig{Cmd: []string{"/bin/sh"}})
	line("Loaded image: " + ref)
	return nil
}

func (f *fakeRunner) SaveImagesArch(ctx context.Context, arch string, w io.Writer, refs ...string) error {
	if arch == "" || arch == hostImageArch {
		return f.SaveImages(ctx, w, refs...)
	}
	_, err := io.WriteString(w, arch+":"+strings.Join(refs, "\n"))
	return err
}

func (f *fakeRunner) PruneImagesArch(ctx context.Context, arch string) (string, error) {
	return "Total reclaimed space: 0B", nil
}
