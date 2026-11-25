package ui

import (
	"path/filepath"
	"testing"
)

func TestVolumeDirWarning(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "u")
	caches := filepath.Join(home, "Library", "Caches", "gvx", "volumes")
	support := filepath.Join(home, "Library", "Application Support", "gvx", "volumes")
	mine := filepath.Join(home, "volumes")
	cacheDir := filepath.Join(home, "gvx-cache")
	notApplied := tr("Volume directory not applied")
	inCache := tr("Volumes are in a cache folder")
	for _, c := range []struct {
		name, goos, dir, cache, volume string
		title, body                    string
	}{
		{"no report", "darwin", "", "", mine, "", ""},
		{"default outside caches", "darwin", support, "", "", "", ""},
		{"setting applied", "darwin", mine, "", mine, "", ""},
		{"setting applied with spaces", "darwin", mine, "", " " + mine + " ", "", ""},
		{"rungv ignores the variable", "darwin", support, "", mine, notApplied,
			tr("This rungv does not read GVX_VOLUME_DIR and keeps volumes in %s. Update rungv to use the setting.", support)},
		{"cache setting wins", "darwin", filepath.Join(cacheDir, "volumes"), cacheDir, mine, notApplied,
			tr("A layer cache directory is set, so rungv keeps volumes in %s. Clear the layer cache directory setting to use the volume directory.", filepath.Join(cacheDir, "volumes"))},
		{"volume setting names the cache volumes", "darwin", filepath.Join(cacheDir, "volumes"), cacheDir, filepath.Join(cacheDir, "volumes"), "", ""},
		{"default in caches", "darwin", caches, "", "", inCache,
			tr("macOS empties ~/Library/Caches when the disk runs low, and named volumes there lose their data. Set a volume directory outside it, or update rungv.")},
		{"cache setting in caches", "darwin", caches, filepath.Dir(caches), "", inCache,
			tr("macOS empties ~/Library/Caches when the disk runs low, and named volumes there lose their data. Move the layer cache directory out of it.")},
		{"caches on linux", "linux", caches, "", "", "", ""},
	} {
		title, body := volumeDirWarning(c.goos, home, c.dir, c.cache, c.volume)
		if title != c.title || body != c.body {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, title, body, c.title, c.body)
		}
	}
}
