//go:build !unix

package engine

func lockVolumeFile(path string, readOnly bool) (func(), error) {
	return func() {}, nil
}
