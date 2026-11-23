// Command bundletool finishes macOS bundles (re-signing nested helper binaries,
// building the DMG, and optionally signing/notarizing with Developer ID) and
// builds Linux .deb/.rpm packages from an assembled install tree that includes
// the bundled Docker CLI, Buildx, and Compose binaries.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/goreleaser/nfpm/v2"
	"github.com/goreleaser/nfpm/v2/deb"
	"github.com/goreleaser/nfpm/v2/files"
	"github.com/goreleaser/nfpm/v2/rpm"
	"github.com/ironpark/zapp/pkg/dmg"
	"github.com/ironpark/zapp/pkg/udif"

	"github.com/ironpark/gvx-desktop/release/bundletool/machosign"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: bundletool <macos-finish|linux-pkg> [flags]\n")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "macos-finish":
		err = runMacOSFinish(os.Args[2:])
	case "linux-pkg":
		err = runLinuxPkg(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundletool: %v\n", err)
		os.Exit(1)
	}
}

type macosSignOpts struct {
	identity        string
	entitlements    string
	p12File         string
	p12Base64       string
	p12Password     string
	p12PasswordFile string
	notarize        bool
	apiKeyFile      string
	notaryProfile   string
	appleID         string
	teamID          string
	notaryPassword  string
	staple          bool
	notaryTimeout   time.Duration
}

func (o macosSignOpts) hasCertificate() bool { return o.p12File != "" || o.p12Base64 != "" }
func (o macosSignOpts) adHoc() bool {
	return (o.identity == "" || o.identity == "-") && !o.hasCertificate()
}

func runMacOSFinish(args []string) error {
	fs := flag.NewFlagSet("macos-finish", flag.ExitOnError)
	appPath := fs.String("app", "", "path to Name.app bundle")
	dmgPath := fs.String("dmg", "", "optional path to output Name.dmg")
	title := fs.String("title", "", "DMG volume title")
	entitlements := fs.String("entitlements", "", "path to entitlements plist")
	identity := fs.String("identity", "-", "signing identity or '-' for ad hoc")
	p12File := fs.String("p12-file", "", "PKCS#12 or PEM certificate file")
	p12Base64Env := fs.String("p12-base64-env", "GGUI_P12_BASE64", "env var holding base64 PKCS#12 certificate")
	p12PasswordFile := fs.String("p12-password-file", "", "file holding PKCS#12 password")
	p12PasswordEnv := fs.String("p12-password-env", "GGUI_P12_PASSWORD", "env var holding PKCS#12 password")
	notarize := fs.Bool("notarize", false, "notarize and staple the app and DMG")
	apiKeyFile := fs.String("api-key-file", "", "App Store Connect API key JSON file")
	notaryProfile := fs.String("notary-profile", "", "notarytool keychain profile")
	appleID := fs.String("apple-id", "", "Apple ID for notarization")
	teamID := fs.String("team-id", "", "Apple Team ID")
	notaryPasswordEnv := fs.String("notary-password-env", "GGUI_NOTARY_PASSWORD", "env var holding app-specific password")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *appPath == "" {
		return fmt.Errorf("--app is required")
	}

	opts := macosSignOpts{
		identity:        *identity,
		entitlements:    *entitlements,
		p12File:         *p12File,
		p12PasswordFile: *p12PasswordFile,
		notarize:        *notarize,
		apiKeyFile:      *apiKeyFile,
		notaryProfile:   *notaryProfile,
		appleID:         *appleID,
		teamID:          *teamID,
		staple:          true,
		notaryTimeout:   time.Hour,
	}
	if opts.p12File == "" && *p12Base64Env != "" {
		opts.p12Base64 = os.Getenv(*p12Base64Env)
	}
	if opts.p12PasswordFile == "" && *p12PasswordEnv != "" {
		opts.p12Password = os.Getenv(*p12PasswordEnv)
	}
	if *notaryPasswordEnv != "" {
		opts.notaryPassword = os.Getenv(*notaryPasswordEnv)
	}

	ctx := context.Background()
	if opts.adHoc() {
		if opts.notarize {
			return fmt.Errorf("notarization requires a Developer ID identity or certificate")
		}
		var ents []byte
		if opts.entitlements != "" {
			var err error
			if ents, err = os.ReadFile(opts.entitlements); err != nil {
				return fmt.Errorf("reading entitlements: %w", err)
			}
		}
		if err := machosign.SignBundle(*appPath, ents); err != nil {
			return fmt.Errorf("ad-hoc signing %s: %w", *appPath, err)
		}
	} else {
		if err := signAndMaybeNotarize(ctx, opts, *appPath, opts.notarize); err != nil {
			return err
		}
	}

	if *dmgPath == "" {
		return nil
	}
	volTitle := *title
	if volTitle == "" {
		volTitle = strings.TrimSuffix(filepath.Base(*appPath), ".app")
	}
	icon := filepath.Join(*appPath, "Contents", "Resources", "AppIcon.icns")
	if _, err := os.Stat(icon); err != nil {
		icon = ""
	}
	const (
		winW, winH = 640, 480
		iconSize   = 128
		labelSize  = 14
	)
	y := winH/2 - iconSize/2 + labelSize
	appX := winW/3 - iconSize/2
	linkX := winW*2/3 + iconSize/2
	hfs, err := dmg.ParseFileSystem("hfsplus")
	if err != nil {
		return err
	}
	udzo, err := udif.ParseFormat("udzo")
	if err != nil {
		return err
	}
	c := dmg.Config{
		FileName:         *dmgPath,
		Title:            volTitle,
		Icon:             icon,
		WindowWidth:      winW,
		WindowHeight:     winH,
		ContentsIconSize: iconSize,
		LabelSize:        labelSize,
		FileSystem:       hfs,
		Format:           udzo,
		Contents: []dmg.Item{
			{Type: dmg.Dir, Path: *appPath, X: appX, Y: y},
			{Type: dmg.Link, Path: "/Applications", X: linkX, Y: y},
		},
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("disk image config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(*dmgPath), 0o755); err != nil {
		return err
	}
	if err := dmg.CreateDMG(ctx, c); err != nil {
		return fmt.Errorf("creating DMG: %w", err)
	}
	if !opts.adHoc() {
		if err := signAndMaybeNotarize(ctx, opts, *dmgPath, opts.notarize); err != nil {
			return err
		}
	}
	return nil
}

type gguiConfig struct {
	App struct {
		Name    string `toml:"name"`
		ID      string `toml:"id"`
		Version string `toml:"version"`
	} `toml:"app"`
	Build struct {
		Renderer string `toml:"renderer"`
	} `toml:"build"`
	Linux struct {
		Description string   `toml:"description"`
		Maintainer  string   `toml:"maintainer"`
		Homepage    string   `toml:"homepage"`
		License     string   `toml:"license"`
		DependsDeb  []string `toml:"depends_deb"`
		DependsRPM  []string `toml:"depends_rpm"`
	} `toml:"linux"`
}

var (
	defaultDependsDeb = []string{
		"libx11-6", "libxrandr2", "libxi6", "libxcursor1", "libxinerama1", "libxext6", "libxrender1",
		"libgl1", "libegl1",
	}
	defaultDependsRPM = []string{
		"libX11", "libXrandr", "libXi", "libXcursor", "libXinerama", "libXext", "libXrender",
		"libglvnd-glx", "libglvnd-egl",
	}
)

const refreshScript = `#!/bin/sh
if command -v update-desktop-database >/dev/null 2>&1; then
	update-desktop-database -q /usr/share/applications || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
	gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true
fi
exit 0
`

func linuxModeFor(rel string) fs.FileMode {
	dir := path.Dir(rel)
	if dir == "usr/bin" ||
		strings.HasSuffix(dir, "/bin") && strings.HasPrefix(dir, "usr/lib/") ||
		strings.HasSuffix(dir, "/cli-plugins") && strings.HasPrefix(dir, "usr/lib/") {
		return 0o755
	}
	return 0o644
}

func runLinuxPkg(args []string) error {
	fsFlags := flag.NewFlagSet("linux-pkg", flag.ExitOnError)
	treeDir := fsFlags.String("tree", "", "Linux install tree directory (containing usr/)")
	outDir := fsFlags.String("outdir", "", "output directory for .deb/.rpm packages")
	cfgPath := fsFlags.String("config", "ggui.toml", "path to ggui.toml")
	pkgName := fsFlags.String("package", "gvx-desktop", "Linux package name")
	arch := fsFlags.String("arch", "amd64", "target architecture (amd64 or arm64)")
	epoch := fsFlags.Int64("epoch", 0, "SOURCE_DATE_EPOCH timestamp")
	formats := fsFlags.String("formats", "deb,rpm", "comma-separated formats (deb,rpm)")
	if err := fsFlags.Parse(args); err != nil {
		return err
	}
	if *treeDir == "" || *outDir == "" {
		return fmt.Errorf("--tree and --outdir are required")
	}
	var cfg gguiConfig
	if _, err := toml.DecodeFile(*cfgPath, &cfg); err != nil {
		return fmt.Errorf("reading %s: %w", *cfgPath, err)
	}
	mtime := time.Unix(*epoch, 0).UTC()
	if *epoch == 0 {
		mtime = time.Now().UTC()
	}

	type item struct {
		rel  string
		src  string
		mode fs.FileMode
	}
	var items []item
	err := filepath.WalkDir(filepath.Join(*treeDir, "usr"), func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(*treeDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		items = append(items, item{rel: rel, src: p, mode: linuxModeFor(rel)})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })

	tmp, err := os.MkdirTemp("", "bundletool-linux-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	scriptPath := filepath.Join(tmp, "refresh.sh")
	if err := os.WriteFile(scriptPath, []byte(refreshScript), 0o755); err != nil {
		return err
	}

	useZig := strings.EqualFold(strings.TrimSpace(cfg.Build.Renderer), "zig")
	dependsFor := func(format string) []string {
		if format == "deb" {
			if len(cfg.Linux.DependsDeb) > 0 {
				return cfg.Linux.DependsDeb
			}
			deps := slices.Clone(defaultDependsDeb)
			if useZig {
				deps = append(deps, "libvulkan1")
			}
			return deps
		}
		if len(cfg.Linux.DependsRPM) > 0 {
			return cfg.Linux.DependsRPM
		}
		deps := slices.Clone(defaultDependsRPM)
		if useZig {
			deps = append(deps, "vulkan-loader")
		}
		return deps
	}

	buildInfo := func(format string) *nfpm.Info {
		contents := make(files.Contents, 0, len(items))
		for _, it := range items {
			contents = append(contents, &files.Content{
				Source:      it.src,
				Destination: "/" + it.rel,
				Type:        files.TypeFile,
				FileInfo:    &files.ContentFileInfo{Mode: it.mode, Owner: "root", Group: "root", MTime: mtime},
			})
		}
		in := &nfpm.Info{
			Name:            *pkgName,
			Arch:            *arch,
			Platform:        "linux",
			Version:         cfg.App.Version,
			VersionSchema:   "none",
			Section:         "misc",
			Maintainer:      cfg.Linux.Maintainer,
			Description:     cfg.Linux.Description,
			Homepage:        cfg.Linux.Homepage,
			License:         cfg.Linux.License,
			DisableGlobbing: true,
			Depends:         dependsFor(format),
			Contents:        contents,
			MTime:           mtime,
			Scripts: nfpm.Scripts{
				PostInstall: scriptPath,
				PostRemove:  scriptPath,
			},
			RPM: nfpm.RPM{Summary: cfg.Linux.Description},
		}
		return nfpm.WithDefaults(in)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	for format := range strings.SplitSeq(*formats, ",") {
		format = strings.TrimSpace(strings.ToLower(format))
		if format == "" {
			continue
		}
		var pk interface {
			nfpm.Packager
			ConventionalFileName(*nfpm.Info) string
		}
		switch format {
		case "deb":
			pk = deb.Default
		case "rpm":
			pk = rpm.DefaultRPM
		default:
			return fmt.Errorf("unsupported format %q", format)
		}
		outFile := filepath.Join(*outDir, pk.ConventionalFileName(buildInfo(format)))
		f, err := os.Create(outFile)
		if err != nil {
			return err
		}
		err = pk.Package(buildInfo(format), f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(outFile)
			return fmt.Errorf("%s package: %w", format, err)
		}
		fmt.Println(outFile)
	}
	return nil
}
