# gvx Desktop

A desktop app for [gvx](https://github.com/gfire-sigs/gvx) — run Linux containers as
unprivileged gvx sandboxes (no VM, no daemon, no root) and manage their images,
volumes and snapshots, in the spirit of Docker Desktop. Built with
[ggui](https://github.com/ironpark/ggui).

설계 배경과 기능 분류는 [docs/DESIGN.md](docs/DESIGN.md)를 참고하세요.

## Features

| Area | What you can do |
| --- | --- |
| Dashboard | Engine status, running/image/volume/snapshot counts, recipe quick start |
| Sandboxes | Create/edit (with a live `rungv run …` preview), start/stop/restart, live logs with filter, published ports (open/copy), duplicate, delete, start with the app |
| Images | List, pull (streaming progress), inspect JSON, run in a new sandbox, remove, prune |
| Volumes | List with in-use state and users, create (ext4lite/hostfs), reveal, delete, prune |
| Snapshots | Checkpoint a running sandbox (stop or keep running), list, restore, delete |
| Docker Engine | One-click `docker:dind`, `docker context` setup, inner container list with start/stop/restart/remove |
| Kubernetes | One-click single-node k3s, kubeconfig detection, kubectl snippets |
| Desktop | Menu-bar tray (macOS/Windows), native menus, ⌘K palette, ⌘1–7 navigation, single instance, light/dark/system theme |

## How it works

gvx has no daemon, and its Go SDK locks down the process that calls it — so the
app never loads gvx itself. It supervises one `rungv` child process per sandbox,
parses its output (`rungv: publishing …`, ready patterns, exit codes) and reads
the management commands through `--format json`. Quitting the app (or SIGINT /
SIGTERM) stops every sandbox it started.

```
internal/rungv      rungv CLI wrapper: locate, info/images/volumes/df, pull/rm/prune, signing
internal/sandbox    Spec → rungv args, Supervisor (processes, logs, ports, ready, checkpoint/restore)
internal/dockerapi  Minimal Docker Engine API client over the dind Unix socket
internal/recipes    Docker Engine and Kubernetes presets
internal/store      sandboxes.json / settings.json in the user config dir
internal/ui         ggui screens
```

## Build and run

Requires Go 1.27+ and a checkout of ggui next to this one (`../ggui`, see `go.mod`).

```sh
go build -o bin/gvx-desktop .
./bin/gvx-desktop
```

The app looks for `rungv` in Settings → rungv path, `$GVX_RUNGV`, `PATH`,
`~/.gvx/bin`, and next to the app / in `./bin`. On macOS rungv must carry the
`com.apple.security.hypervisor` entitlement; the app detects an unsigned binary
and can sign it (Settings → Sign rungv).

Render every screen with demo data, without an engine:

```sh
./bin/gvx-desktop -render-dir ./shots
./bin/gvx-desktop -render-dir ./shots-ko -render-lang ko
```

The app is worded in English and Korean. It follows the system language, and
Settings → Language chooses one. The Korean text lives in
`internal/ui/i18n_ko.go`, keyed by the English in the code; a test fails when
a text has no Korean.

## Tests

```sh
go test -short ./...                       # unit tests
go test ./internal/sandbox/                # + real sandboxes (needs a working rungv)
GVXD_DOCKER_TEST=1 go test ./internal/recipes/   # + boots docker:dind and queries its API
```
