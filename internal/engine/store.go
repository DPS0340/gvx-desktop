package engine

// Owner: frozen (architect). Each store file has one owner: containers.json
// is workstream B's, volumes.json and networks.json workstream C's, and the
// logs directory workstream A's.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// storeVersion is the version field of every store file.
const storeVersion = 1

// path is a file under the engine's directory.
func (e *Engine) path(elem ...string) string {
	return filepath.Join(append([]string{e.cfg.Dir}, elem...)...)
}

// loadFile reads JSON from path into v; a missing file leaves v as it is.
func loadFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// saveFile replaces path with the JSON of v atomically.
func saveFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
