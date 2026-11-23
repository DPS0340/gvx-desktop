#!/bin/sh
# Downloads, verifies (SHA-256), and stages the pinned official Docker CLI,
# Buildx, and Compose binaries for a target OS/architecture.
#
# Usage:
#   release/fetch-docker-cli.sh <darwin|linux|windows> <amd64|arm64> <OUTDIR>
#
# Writes:
#   <OUTDIR>/docker[.exe]
#   <OUTDIR>/cli-plugins/docker-buildx[.exe]
#   <OUTDIR>/cli-plugins/docker-compose[.exe]
set -eu
umask 022

usage='usage: fetch-docker-cli.sh <darwin|linux|windows> <amd64|arm64> <OUTDIR>'
goos=${1:?$usage}
goarch=${2:?$usage}
outdir=${3:?$usage}

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=release/docker-cli.env
. "$repo_root/release/docker-cli.env"

cache_dir=${GVX_DOCKER_CLI_CACHE:-"${HOME:-/tmp}/.cache/gvx-desktop/docker-cli"}
mkdir -p "$cache_dir" "$outdir/cli-plugins"

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		echo "fetch-docker-cli: neither sha256sum nor shasum found on PATH" >&2
		exit 1
	fi
}

fetch_verified() {
	url=$1
	want=$2
	cache_name=$3
	dest="$cache_dir/$cache_name"
	if [ -f "$dest" ]; then
		got=$(sha256_of "$dest")
		if [ "$got" = "$want" ]; then
			printf '%s\n' "$dest"
			return
		fi
		rm -f "$dest"
	fi
	tmp="$dest.tmp.$$"
	trap 'rm -f "$tmp"' EXIT
	echo "fetch-docker-cli: downloading $cache_name" >&2
	curl -fsSL --retry 3 --retry-delay 2 -o "$tmp" "$url"
	got=$(sha256_of "$tmp")
	if [ "$got" != "$want" ]; then
		rm -f "$tmp"
		echo "fetch-docker-cli: SHA-256 mismatch for $cache_name: got $got, want $want" >&2
		exit 1
	fi
	mv -f "$tmp" "$dest"
	trap - EXIT
	printf '%s\n' "$dest"
}

ext=""
case "$goos/$goarch" in
darwin/arm64)
	cli_url="https://download.docker.com/mac/static/stable/aarch64/docker-cli_${DOCKER_CLI_VERSION}.tgz"
	cli_sha="$DOCKER_CLI_SHA256_DARWIN_ARM64"
	cli_file="docker-cli_${DOCKER_CLI_VERSION}-darwin-arm64.tgz"
	bx_url="https://github.com/docker/buildx/releases/download/${DOCKER_BUILDX_VERSION}/buildx-${DOCKER_BUILDX_VERSION}.darwin-arm64"
	bx_sha="$DOCKER_BUILDX_SHA256_DARWIN_ARM64"
	bx_file="buildx-${DOCKER_BUILDX_VERSION}.darwin-arm64"
	comp_url="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-darwin-aarch64"
	comp_sha="$DOCKER_COMPOSE_SHA256_DARWIN_ARM64"
	comp_file="docker-compose-${DOCKER_COMPOSE_VERSION}-darwin-aarch64"
	;;
linux/amd64)
	cli_url="https://download.docker.com/linux/static/stable/x86_64/docker-cli_${DOCKER_CLI_VERSION}.tgz"
	cli_sha="$DOCKER_CLI_SHA256_LINUX_AMD64"
	cli_file="docker-cli_${DOCKER_CLI_VERSION}-linux-amd64.tgz"
	bx_url="https://github.com/docker/buildx/releases/download/${DOCKER_BUILDX_VERSION}/buildx-${DOCKER_BUILDX_VERSION}.linux-amd64"
	bx_sha="$DOCKER_BUILDX_SHA256_LINUX_AMD64"
	bx_file="buildx-${DOCKER_BUILDX_VERSION}.linux-amd64"
	comp_url="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-x86_64"
	comp_sha="$DOCKER_COMPOSE_SHA256_LINUX_AMD64"
	comp_file="docker-compose-${DOCKER_COMPOSE_VERSION}-linux-x86_64"
	;;
linux/arm64)
	cli_url="https://download.docker.com/linux/static/stable/aarch64/docker-cli_${DOCKER_CLI_VERSION}.tgz"
	cli_sha="$DOCKER_CLI_SHA256_LINUX_ARM64"
	cli_file="docker-cli_${DOCKER_CLI_VERSION}-linux-arm64.tgz"
	bx_url="https://github.com/docker/buildx/releases/download/${DOCKER_BUILDX_VERSION}/buildx-${DOCKER_BUILDX_VERSION}.linux-arm64"
	bx_sha="$DOCKER_BUILDX_SHA256_LINUX_ARM64"
	bx_file="buildx-${DOCKER_BUILDX_VERSION}.linux-arm64"
	comp_url="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-aarch64"
	comp_sha="$DOCKER_COMPOSE_SHA256_LINUX_ARM64"
	comp_file="docker-compose-${DOCKER_COMPOSE_VERSION}-linux-aarch64"
	;;
windows/amd64)
	ext=".exe"
	cli_url="https://download.docker.com/win/static/stable/x86_64/docker-cli_${DOCKER_CLI_VERSION}.zip"
	cli_sha="$DOCKER_CLI_SHA256_WINDOWS_AMD64"
	cli_file="docker-cli_${DOCKER_CLI_VERSION}-windows-amd64.zip"
	bx_url="https://github.com/docker/buildx/releases/download/${DOCKER_BUILDX_VERSION}/buildx-${DOCKER_BUILDX_VERSION}.windows-amd64.exe"
	bx_sha="$DOCKER_BUILDX_SHA256_WINDOWS_AMD64"
	bx_file="buildx-${DOCKER_BUILDX_VERSION}.windows-amd64.exe"
	comp_url="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-windows-x86_64.exe"
	comp_sha="$DOCKER_COMPOSE_SHA256_WINDOWS_AMD64"
	comp_file="docker-compose-${DOCKER_COMPOSE_VERSION}-windows-x86_64.exe"
	;;
windows/arm64)
	ext=".exe"
	cli_url="https://download.docker.com/win/static/stable/aarch64/docker-cli_${DOCKER_CLI_VERSION}.zip"
	cli_sha="$DOCKER_CLI_SHA256_WINDOWS_ARM64"
	cli_file="docker-cli_${DOCKER_CLI_VERSION}-windows-arm64.zip"
	bx_url="https://github.com/docker/buildx/releases/download/${DOCKER_BUILDX_VERSION}/buildx-${DOCKER_BUILDX_VERSION}.windows-arm64.exe"
	bx_sha="$DOCKER_BUILDX_SHA256_WINDOWS_ARM64"
	bx_file="buildx-${DOCKER_BUILDX_VERSION}.windows-arm64.exe"
	comp_url="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-windows-aarch64.exe"
	comp_sha="$DOCKER_COMPOSE_SHA256_WINDOWS_ARM64"
	comp_file="docker-compose-${DOCKER_COMPOSE_VERSION}-windows-aarch64.exe"
	;;
*)
	echo "fetch-docker-cli: unsupported target $goos/$goarch" >&2
	exit 1
	;;
esac

cli_archive=$(fetch_verified "$cli_url" "$cli_sha" "$cli_file")
bx_bin=$(fetch_verified "$bx_url" "$bx_sha" "$bx_file")
comp_bin=$(fetch_verified "$comp_url" "$comp_sha" "$comp_file")

extract_tmp=$(mktemp -d)
trap 'rm -rf "$extract_tmp"' EXIT

if [ "$goos" = windows ]; then
	unzip -q -o "$cli_archive" "bin/docker.exe" -d "$extract_tmp"
else
	tar -xzf "$cli_archive" -C "$extract_tmp" "bin/docker"
fi

cp -f "$extract_tmp/bin/docker$ext" "$outdir/docker$ext"
cp -f "$bx_bin" "$outdir/cli-plugins/docker-buildx$ext"
cp -f "$comp_bin" "$outdir/cli-plugins/docker-compose$ext"
chmod 0755 \
	"$outdir/docker$ext" \
	"$outdir/cli-plugins/docker-buildx$ext" \
	"$outdir/cli-plugins/docker-compose$ext"
