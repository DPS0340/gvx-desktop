//go:build !darwin && !(cgo && (amd64 || arm64) && (linux || (windows && ggui_rcodesign)))

package main

import (
	"context"
	"fmt"
)

func signAndMaybeNotarize(context.Context, macosSignOpts, string, bool) error {
	return fmt.Errorf("bundletool was built without cgo/rcodesign and cannot sign with a Developer ID certificate on this host; rebuild with CGO_ENABLED=1")
}
