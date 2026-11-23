#!/bin/sh
# Deterministic build driver for GVX Desktop releases and local verification.
#
# Usage:
#   release/repro-build.sh install-ggui [BINDIR]
#   release/repro-build.sh binary GOOS GOARCH VERSION OUTPUT
#   release/repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]
#   release/repro-build.sh verify-binary BINARY GOOS GOARCH
#
# Reads pinned toolchain settings from release/toolchain.env, normalizes umask,
# scrubs ambient environment variables, disables cgo for application builds,
# strips paths and build IDs, and packs Windows zips and Linux tarballs with
# release/repro-pack.go using SOURCE_DATE_EPOCH so archives match bit for bit
# across Linux and macOS hosts.
set -eu
umask 022

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=release/toolchain.env
. "$repo_root/release/toolchain.env"

go_bin=${GO:-go}
go_path=$(command -v "$go_bin") || { echo "repro-build: go not found on PATH" >&2; exit 1; }
gobin_dir=$(dirname -- "$go_path")
goproxy=${GVX_DESKTOP_GOPROXY:-$GOPROXY}
gosumdb=${GVX_DESKTOP_GOSUMDB:-$GOSUMDB}
toolchain="go$GO_VERSION"
gomodcache=${GOMODCACHE:-$("$go_bin" env GOMODCACHE)}
gocache=${GOCACHE:-$("$go_bin" env GOCACHE)}
home_dir=${HOME:-/tmp}
tmp_dir=${TMPDIR:-/tmp}

section_field() {
	awk -v sec="[$1]" -v key="$2" '
		{ line = $0; sub(/[[:space:]]*$/, "", line) }
		line == sec { in_sec = 1; next }
		/^\[/ { in_sec = 0 }
		in_sec && index($0, key " = \"") == 1 {
			sub(/^[^"]*"/, "")
			sub(/"[[:space:]]*$/, "")
			print
			exit
		}
	' "$repo_root/ggui.toml"
}

app_name=$(section_field app name)
app_id=$(section_field app id)
app_version=$(section_field app version)
build_renderer=$(section_field build renderer)
[ -n "$app_name" ] || { echo "repro-build: ggui.toml has no [app] name" >&2; exit 1; }
[ -n "$app_id" ] || { echo "repro-build: ggui.toml has no [app] id" >&2; exit 1; }
[ -n "$app_version" ] || { echo "repro-build: ggui.toml has no [app] version" >&2; exit 1; }

resolve_epoch() {
	if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
		printf '%s\n' "$SOURCE_DATE_EPOCH"
		return
	fi
	git -C "$repo_root" log -1 --format=%ct
}

arch_goamd64() {
	case "$1" in
	linux) printf '%s\n' "$GOAMD64_LINUX" ;;
	windows) printf '%s\n' "$GOAMD64_WINDOWS" ;;
	*) printf '%s\n' "$GOAMD64_DEFAULT" ;;
	esac
}

arch_goarm64() {
	case "$1" in
	darwin|macos) printf '%s\n' "$GOARM64_DARWIN" ;;
	windows) printf '%s\n' "$GOARM64_WINDOWS" ;;
	*) printf '%s\n' "$GOARM64_LINUX" ;;
	esac
}

run_repro_pack() {
	env -i \
		PATH="$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
		HOME="$home_dir" \
		TMPDIR="$tmp_dir" \
		LC_ALL=C \
		TZ=UTC \
		GOMODCACHE="$gomodcache" \
		GOCACHE="$gocache" \
		GOENV=off \
		GOWORK=off \
		GO111MODULE=on \
		GOPROXY="$goproxy" \
		GOSUMDB="$gosumdb" \
		GOTOOLCHAIN="$toolchain" \
		GOTELEMETRY=off \
		GOFIPS140=off \
		GOEXPERIMENT= \
		GODEBUG= \
		GOFLAGS= \
		CGO_ENABLED=0 \
		${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
		${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
		${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
		"$go_bin" run "$repo_root/release/repro-pack.go" "$@"
}

cmd=${1:?usage: repro-build.sh <install-ggui|binary|package|verify-binary> ...}
shift

case "$cmd" in
install-ggui)
	bindir=${1:-$("$go_bin" env GOPATH)/bin}
	mkdir -p -- "$bindir"
	bindir_abs=$(CDPATH= cd -- "$bindir" && pwd)
	ggui_cgo=${GGUI_CGO:-0}
	exec env -i \
		PATH="$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
		HOME="$home_dir" \
		TMPDIR="$tmp_dir" \
		LC_ALL=C \
		TZ=UTC \
		GOMODCACHE="$gomodcache" \
		GOCACHE="$gocache" \
		GOBIN="$bindir_abs" \
		GOENV=off \
		GOWORK=off \
		GO111MODULE=on \
		GOPROXY="$goproxy" \
		GOSUMDB="$gosumdb" \
		GOTOOLCHAIN="$toolchain" \
		GOTELEMETRY=off \
		GOFIPS140=off \
		GOEXPERIMENT= \
		GODEBUG= \
		GOFLAGS= \
		CGO_ENABLED="$ggui_cgo" \
		${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
		${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
		${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
		"$go_bin" install \
			-trimpath \
			-pgo=off \
			-compiler=gc \
			-ldflags="-s -w -buildid=" \
			-- "github.com/ironpark/ggui/cmd/ggui@$GGUI_VERSION"
	;;

binary)
	goos=${1:?usage: repro-build.sh binary GOOS GOARCH VERSION OUTPUT}
	goarch=${2:?usage: repro-build.sh binary GOOS GOARCH VERSION OUTPUT}
	version=${3:?usage: repro-build.sh binary GOOS GOARCH VERSION OUTPUT}
	out=${4:?usage: repro-build.sh binary GOOS GOARCH VERSION OUTPUT}
	mkdir -p -- "$(dirname -- "$out")"
	out_abs="$(CDPATH= cd -- "$(dirname -- "$out")" && pwd)/$(basename -- "$out")"
	goamd64=$(arch_goamd64 "$goos")
	goarm64=$(arch_goarm64 "$goos")
	ldflags="-s -w -buildid= -X main.version=$version -X github.com/ironpark/ggui.buildID=$app_id"
	if [ "$goos" = windows ] && [ "${WINDOWS_GUI:-0}" = 1 ]; then
		ldflags="$ldflags -H=windowsgui"
	fi
	set --
	if [ "$build_renderer" = zig ]; then
		case "$goos/$goarch" in
		darwin/amd64|darwin/arm64|windows/amd64|windows/arm64|linux/amd64|linux/arm64)
			set -- -tags=ggfxzig
			;;
		esac
	fi
	exec env -i \
		PATH="$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
		HOME="$home_dir" \
		TMPDIR="$tmp_dir" \
		LC_ALL=C \
		TZ=UTC \
		GOMODCACHE="$gomodcache" \
		GOCACHE="$gocache" \
		GOENV=off \
		GOWORK=off \
		GO111MODULE=on \
		GOPROXY="$goproxy" \
		GOSUMDB="$gosumdb" \
		GOTOOLCHAIN="$toolchain" \
		GOTELEMETRY=off \
		GOFIPS140=off \
		GOEXPERIMENT= \
		GODEBUG= \
		GOFLAGS= \
		CGO_ENABLED=0 \
		GOOS="$goos" \
		GOARCH="$goarch" \
		GOAMD64="$goamd64" \
		GOARM64="$goarm64" \
		${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
		${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
		${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
		"$go_bin" build \
			-C "$repo_root" \
			-mod=readonly \
			-pgo=off \
			-buildvcs=false \
			-trimpath \
			-buildmode=exe \
			-compiler=gc \
			"$@" \
			-ldflags="$ldflags" \
			-o "$out_abs" \
			.
	;;

package)
	target=${1:?usage: repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]}
	arch=${2:?usage: repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]}
	version=${3:?usage: repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]}
	outdir=${4:?usage: repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]}
	shift 4

	if [ "$version" != "$app_version" ]; then
		echo "repro-build: version argument $version does not match ggui.toml ($app_version)" >&2
		exit 1
	fi

	if [ -n "${GGUI:-}" ]; then
		ggui_bin=$GGUI
	elif [ -x "$("$go_bin" env GOPATH)/bin/ggui" ]; then
		ggui_bin="$("$go_bin" env GOPATH)/bin/ggui"
	else
		ggui_bin=$(command -v ggui 2>/dev/null || true)
	fi
	[ -n "$ggui_bin" ] && [ -x "$ggui_bin" ] || {
		echo "repro-build: ggui not found; run ./release/repro-build.sh install-ggui first" >&2
		exit 1
	}

	ggui_meta=$(GOTOOLCHAIN="$toolchain" "$go_bin" version -m "$ggui_bin")
	if ! printf '%s\n' "$ggui_meta" | awk -v ver="$GGUI_VERSION" '$1 == "mod" && $2 == "github.com/ironpark/ggui/cmd/ggui" && $3 == ver { found = 1 } END { exit !found }'; then
		echo "repro-build: $ggui_bin does not match pinned ggui $GGUI_VERSION; run ./release/repro-build.sh install-ggui" >&2
		exit 1
	fi

	ggui_dir=$(dirname -- "$ggui_bin")
	epoch=$(resolve_epoch)
	goamd64=$(arch_goamd64 "$target")
	goarm64=$(arch_goarm64 "$target")
	mkdir -p -- "$outdir"
	outdir_abs=$(CDPATH= cd -- "$outdir" && pwd)

	run_ggui() {
		env -i \
			PATH="$ggui_dir:$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
			HOME="$home_dir" \
			TMPDIR="$tmp_dir" \
			LC_ALL=C \
			TZ=UTC \
			SOURCE_DATE_EPOCH="$epoch" \
			GOMODCACHE="$gomodcache" \
			GOCACHE="$gocache" \
			GOENV=off \
			GOWORK=off \
			GO111MODULE=on \
			GOPROXY="$goproxy" \
			GOSUMDB="$gosumdb" \
			GOTOOLCHAIN="$toolchain" \
			GOTELEMETRY=off \
			GOFIPS140=off \
			GOEXPERIMENT= \
			GODEBUG= \
			GOFLAGS="-mod=readonly -pgo=off -buildvcs=false" \
			CGO_ENABLED=0 \
			GOARCH="$arch" \
			GOAMD64="$goamd64" \
			GOARM64="$goarm64" \
			${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
			${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
			${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
			${GGUI_P12_BASE64:+GGUI_P12_BASE64="$GGUI_P12_BASE64"} \
			${GGUI_P12_PASSWORD:+GGUI_P12_PASSWORD="$GGUI_P12_PASSWORD"} \
			${GGUI_NOTARY_PASSWORD:+GGUI_NOTARY_PASSWORD="$GGUI_NOTARY_PASSWORD"} \
			"$ggui_bin" "$@"
	}

	run_bundletool() {
		bt_cgo=${GGUI_CGO:-0}
		env -i \
			PATH="$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
			HOME="$home_dir" \
			TMPDIR="$tmp_dir" \
			LC_ALL=C \
			TZ=UTC \
			SOURCE_DATE_EPOCH="$epoch" \
			GOMODCACHE="$gomodcache" \
			GOCACHE="$gocache" \
			GOENV=off \
			GOWORK=off \
			GO111MODULE=on \
			GOPROXY="$goproxy" \
			GOSUMDB="$gosumdb" \
			GOTOOLCHAIN="$toolchain" \
			GOTELEMETRY=off \
			GOFIPS140=off \
			GOEXPERIMENT= \
			GODEBUG= \
			GOFLAGS="-mod=readonly" \
			CGO_ENABLED="$bt_cgo" \
			${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
			${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
			${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
			${GGUI_P12_BASE64:+GGUI_P12_BASE64="$GGUI_P12_BASE64"} \
			${GGUI_P12_PASSWORD:+GGUI_P12_PASSWORD="$GGUI_P12_PASSWORD"} \
			${GGUI_NOTARY_PASSWORD:+GGUI_NOTARY_PASSWORD="$GGUI_NOTARY_PASSWORD"} \
			"$go_bin" run -C "$repo_root/release/bundletool" . "$@"
	}

	case "$target" in
	macos)
		if [ "$(basename -- "$repo_root")" != "gvx-desktop" ]; then
			echo "repro-build: warning: repo directory is $(basename -- "$repo_root"), not gvx-desktop; CFBundleExecutable follows the directory name" >&2
		fi
		(
			cd "$repo_root"
			# Build the .app bundle without creating/notarizing the DMG yet so we
			# can bundle the Docker CLI helpers inside Contents/Helpers first,
			# then sign the complete bundle and build the DMG in one pass.
			run_ggui build --target macos --dmg=false --installer=false --out "$outdir_abs" --ldflags "-buildid= -X main.version=$version"
			"$repo_root/release/fetch-docker-cli.sh" darwin "$arch" "$outdir_abs/$app_name.app/Contents/Helpers"
			run_bundletool macos-finish \
				--app "$outdir_abs/$app_name.app" \
				--dmg "$outdir_abs/$app_name.dmg" \
				--title "$app_name" \
				--entitlements "$repo_root/assets/entitlements.plist" \
				"$@"
			"$repo_root/release/check-bundle.sh" "$outdir_abs"
		)
		dmg="gvx-desktop-$version-macos-$arch.dmg"
		mv -f "$outdir_abs/$app_name.dmg" "$outdir_abs/$dmg"
		;;
	windows)
		cli_tmp=$(mktemp -d)
		win_extra=$(mktemp -d)
		zip_stage=$(mktemp -d)
		trap 'rm -rf "$cli_tmp" "$win_extra" "$zip_stage"' EXIT
		"$repo_root/release/fetch-docker-cli.sh" windows "$arch" "$cli_tmp"
		mkdir -p "$win_extra/bin" "$win_extra/cli-plugins"
		cp -f "$cli_tmp/docker.exe" "$win_extra/bin/docker.exe"
		cp -f "$cli_tmp/cli-plugins/docker-buildx.exe" "$win_extra/cli-plugins/docker-buildx.exe"
		cp -f "$cli_tmp/cli-plugins/docker-compose.exe" "$win_extra/cli-plugins/docker-compose.exe"
		(
			cd "$repo_root"
			run_ggui build --target windows --installer --installer-extra-dir "$win_extra" --out "$outdir_abs" --ldflags "-buildid= -X main.version=$version" "$@"
		)
		exe="$outdir_abs/$app_name.exe"
		dll="$outdir_abs/ggfx_driver.dll"
		setup_raw="$outdir_abs/$app_name-Setup.exe"
		if [ ! -f "$dll" ] && [ "$build_renderer" = zig ]; then
			echo "repro-build: expected $dll beside $exe for renderer=zig" >&2
			exit 1
		fi
		if [ ! -f "$setup_raw" ]; then
			echo "repro-build: expected $setup_raw from ggui build --target windows --installer" >&2
			exit 1
		fi
		setup="$outdir_abs/gvx-desktop-$version-windows-$arch-setup.exe"
		mv -f "$setup_raw" "$setup"
		mkdir -p "$outdir_abs/bin" "$outdir_abs/cli-plugins" "$zip_stage/bin" "$zip_stage/cli-plugins"
		cp -f "$win_extra/bin/docker.exe" "$outdir_abs/bin/docker.exe"
		cp -f "$win_extra/cli-plugins/docker-buildx.exe" "$outdir_abs/cli-plugins/docker-buildx.exe"
		cp -f "$win_extra/cli-plugins/docker-compose.exe" "$outdir_abs/cli-plugins/docker-compose.exe"
		cp -f "$exe" "$zip_stage/$app_name.exe"
		if [ -f "$dll" ]; then
			cp -f "$dll" "$zip_stage/ggfx_driver.dll"
		fi
		cp -f "$outdir_abs/bin/docker.exe" "$zip_stage/bin/docker.exe"
		cp -f "$outdir_abs/cli-plugins/docker-buildx.exe" "$zip_stage/cli-plugins/docker-buildx.exe"
		cp -f "$outdir_abs/cli-plugins/docker-compose.exe" "$zip_stage/cli-plugins/docker-compose.exe"
		zip="$outdir_abs/gvx-desktop-$version-windows-$arch.zip"
		rm -f "$zip"
		run_repro_pack zip "$epoch" "$zip" "$zip_stage"
		rm -rf "$cli_tmp" "$win_extra" "$zip_stage"
		trap - EXIT
		;;
	linux)
		(
			cd "$repo_root"
			run_ggui build --target linux --format "" --out "$outdir_abs" --ldflags "-buildid= -X main.version=$version" "$@"
		)
		tree="$outdir_abs/gvx-desktop-linux-$arch"
		if [ "$build_renderer" = zig ] && [ ! -f "$tree/usr/lib/gvx-desktop/libggfx_driver.so" ]; then
			echo "repro-build: expected $tree/usr/lib/gvx-desktop/libggfx_driver.so for renderer=zig" >&2
			exit 1
		fi
		cli_tmp=$(mktemp -d)
		trap 'rm -rf "$cli_tmp"' EXIT
		"$repo_root/release/fetch-docker-cli.sh" linux "$arch" "$cli_tmp"
		mkdir -p "$tree/usr/lib/gvx-desktop/bin" "$tree/usr/lib/gvx-desktop/cli-plugins"
		cp -f "$cli_tmp/docker" "$tree/usr/lib/gvx-desktop/bin/docker"
		cp -f "$cli_tmp/cli-plugins/docker-buildx" "$tree/usr/lib/gvx-desktop/cli-plugins/docker-buildx"
		cp -f "$cli_tmp/cli-plugins/docker-compose" "$tree/usr/lib/gvx-desktop/cli-plugins/docker-compose"
		chmod 0755 \
			"$tree/usr/lib/gvx-desktop/bin/docker" \
			"$tree/usr/lib/gvx-desktop/cli-plugins/docker-buildx" \
			"$tree/usr/lib/gvx-desktop/cli-plugins/docker-compose"
		rm -rf "$cli_tmp"
		trap - EXIT
		tarball="$outdir_abs/gvx-desktop-$version-linux-$arch.tar.gz"
		rm -f "$tarball"
		run_repro_pack tar "$epoch" "$tarball" "$tree"
		run_bundletool linux-pkg \
			--tree "$tree" \
			--outdir "$outdir_abs" \
			--config "$repo_root/ggui.toml" \
			--arch "$arch" \
			--epoch "$epoch" \
			--formats deb,rpm
		;;
	*)
		echo "repro-build: unknown target '$target' (expected macos, windows, or linux)" >&2
		exit 1
		;;
	esac
	;;

verify-binary)
	bin=${1:?usage: repro-build.sh verify-binary BINARY GOOS GOARCH}
	want_os=${2:?usage: repro-build.sh verify-binary BINARY GOOS GOARCH}
	want_arch=${3:?usage: repro-build.sh verify-binary BINARY GOOS GOARCH}
	meta=$(GOTOOLCHAIN="$toolchain" "$go_bin" version -m "$bin")
	fail() {
		echo "repro-build: verify-binary failed for $bin: $1" >&2
		printf '%s\n' "$meta" >&2
		exit 1
	}
	printf '%s\n' "$meta" | head -n 1 | grep -Fq "$toolchain" || fail "expected toolchain $toolchain"
	printf '%s\n' "$meta" | grep -Fq -- "-buildmode=exe" || fail "expected -buildmode=exe"
	printf '%s\n' "$meta" | grep -Fq -- "-compiler=gc" || fail "expected -compiler=gc"
	printf '%s\n' "$meta" | grep -Fq -- "-trimpath=true" || fail "expected -trimpath=true"
	printf '%s\n' "$meta" | grep -Fq "CGO_ENABLED=0" || fail "expected CGO_ENABLED=0"
	printf '%s\n' "$meta" | grep -Fq "GOOS=$want_os" || fail "expected GOOS=$want_os"
	printf '%s\n' "$meta" | grep -Fq "GOARCH=$want_arch" || fail "expected GOARCH=$want_arch"
	if [ "$want_arch" = amd64 ]; then
		want_level="GOAMD64=$(arch_goamd64 "$want_os")"
		printf '%s\n' "$meta" | grep -Fq "$want_level" || fail "expected $want_level"
	elif [ "$want_arch" = arm64 ]; then
		want_level="GOARM64=$(arch_goarm64 "$want_os")"
		printf '%s\n' "$meta" | grep -Fq "$want_level" || fail "expected $want_level"
	else
		want_level="default"
	fi
	if printf '%s\n' "$meta" | grep -Eq '^[[:space:]]+build[[:space:]]+vcs\.'; then
		fail "unexpected VCS metadata embedded in binary"
	fi
	bid=$(GOTOOLCHAIN="$toolchain" "$go_bin" tool buildid "$bin")
	if [ -n "$bid" ]; then
		fail "expected empty Go build ID (-buildid=), got '$bid'"
	fi
	echo "verify-binary: $bin ($toolchain $want_os/$want_arch, $want_level, buildid=empty)"
	;;

*)
	echo "repro-build: unknown command '$cmd'" >&2
	exit 1
	;;
esac
