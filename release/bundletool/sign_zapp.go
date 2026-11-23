//go:build darwin || (cgo && (amd64 || arm64) && (linux || (windows && ggui_rcodesign)))

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ironpark/zapp/pkg/signing"
	zmacos "github.com/ironpark/zapp/pkg/signing/macos"
)

var fingerprintRe = regexp.MustCompile(`^[0-9A-Fa-f]{40}$`)

func selectSigner(opts macosSignOpts) (signing.Backend, error) {
	c := signing.Credentials{
		Entitlements:    opts.entitlements,
		P12File:         opts.p12File,
		P12Base64:       opts.p12Base64,
		P12Password:     opts.p12Password,
		P12PasswordFile: opts.p12PasswordFile,
		APIKeyFile:      opts.apiKeyFile,
		Profile:         opts.notaryProfile,
		AppleID:         opts.appleID,
		TeamID:          opts.teamID,
		Password:        opts.notaryPassword,
		NotarizeTimeout: opts.notaryTimeout,
	}
	if c.P12File != "" && isPEM(c.P12File) {
		c.PEMFile = c.P12File
		c.P12File, c.P12Password, c.P12PasswordFile = "", "", ""
	}
	if runtime.GOOS == "darwin" {
		id := opts.identity
		if opts.hasCertificate() && id == "-" {
			id = ""
		}
		if fingerprintRe.MatchString(id) && !opts.hasCertificate() {
			if ids, err := zmacos.ListIdentities(context.Background()); err == nil {
				for _, item := range ids {
					if strings.EqualFold(item.Fingerprint, id) {
						id = item.Description
						break
					}
				}
			}
		}
		c.Identity = id
	}
	return signing.Select(c)
}

func signAndMaybeNotarize(ctx context.Context, opts macosSignOpts, target string, notarize bool) error {
	b, err := selectSigner(opts)
	if err != nil {
		return err
	}
	defer signing.Close(b)
	if err := b.Sign(ctx, target); err != nil {
		return err
	}
	if notarize {
		if err := signing.Notarize(ctx, b, target, opts.staple); err != nil {
			return err
		}
	}
	return nil
}

func isPEM(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return bytes.HasPrefix(bytes.TrimLeft(head[:n], "\ufeff \t\r\n"), []byte("-----BEGIN "))
}

var _ = time.Second
var _ = fmt.Sprintf
