# amd64 builder VM (experimental)

Status: design and compiling skeleton. Branch `feat/amd64-builder`.
Requirements: the coordinator's `amd64-builder-requirements.md`. Its SAFETY
section binds every workstream. In short: the user's real gvx sandboxes
(docker-engine, k3s, registry) keep running. Never touch them, `~/.gvx`, the
app's data, `~/.docker`, `~/.kube` or the real rungv cache. Never launch the
app normally. Kill only your own processes. Clean up VMs, sockets and temp
dirs. Keep work directories short (`/tmp/gvxd-…`): a Unix socket path may be
104 bytes at most. Nobody commits, pushes, switches branches or bumps versions.

> The VM side now lives in its own repository, [ironpark/gvx-desktop-vm](https://github.com/ironpark/gvx-desktop-vm): `internal/vz`, `internal/vmhelper`, `internal/vmproto`, `internal/vmclient`, `internal/guestproto`, `internal/vmassets`, `guest/**`, `cmd/gvx-vmhelper`, `experiments/` and the `vm-assets` workflow below are its `vz`, `vmhelper`, `vmproto`, `vmclient`, `guestproto`, `vmassets`, `guest/**`, `cmd/gvx-vmhelper`, `experiments/` and `.github/workflows/vm-assets.yml`. The VM assets are released there. This document keeps the old paths.

## 1. What it is

The sandbox engine (`docs/design/sandbox-engine.md`) builds images with a lazy
buildkitd sandbox behind the buildx remote builder `gvx-desktop`. gvx sandboxes
run arm64 only: there is no binfmt_misc in gVisor and the guest architecture
is the host's. Rosetta for Linux works only in a Virtualization.framework (VZ)
VM. So the amd64 builder is a **second node** of the `gvx-desktop` builder,
for `linux/amd64` only. A small VZ Linux VM serves it, with Rosetta, buildkitd
and a persistent disk, run by a **helper process**. A build for
`--platform linux/amd64` goes to the VM. A native one stays on the gvx node. A
multi-platform build is split between the two.

v1 builds only. An amd64 image can be loaded into the engine (`--load`),
listed, inspected, tagged, saved and removed, or pushed straight from buildkitd
(`--push`). `docker run` of one is refused with a clear message. The helper
and agent protocols, and the VM lifecycle, assume nothing about buildkitd
beyond a service name, so that amd64 containers can come later (§15).

The prototype, `experiments/vzrosetta` (its README has every measurement),
proved the approach with cgo. It is now a Go module of its own
(`experiments/vzrosetta/go.mod`; build with `GOWORK=off`, as `build.sh`
does), so the app's `go.mod` no longer carries vz.

## 2. Facts this design relies on (verified on this Mac)

M1 Ultra, macOS 27.2.1, Rosetta installed, Go 1.27.4, docker 29.10, buildx
0.39.1, rungv v0.18.

**Pure Go VZ works** (the spike: `CGO_ENABLED=0`, purego v0.11.2 +
`purego/objc`; the code is `experiments/vzpurego`, a module of its own,
400 lines):

- The native build and the `GOOS=darwin GOARCH=arm64` cross build are
  **byte-identical**, and `go version -m` says `CGO_ENABLED=0`. The binary links
  only libSystem and libresolv. Frameworks are `dlopen`ed at run time.
- Ad-hoc signed with `com.apple.security.virtualization`, it boots the
  prototype's kernel and initramfs. The agent accepts on vsock 0.38 s after the
  start (0.33 s with cgo vz).
- What worked: the configuration objects (VZLinuxBootLoader with command line
  and initrd, the virtio console on NSFileHandle pipes, entropy, the
  traditional balloon, vsock, NAT with a random MAC,
  VZLinuxRosettaDirectoryShare, a VZSingleDirectoryShare, a
  VZDiskImageStorageDeviceAttachment as virtio-blk). Also:
  - `validateWithError:` with an `NSError**` out-param: an empty config
    answers `Invalid virtual machine configuration. "bootLoader" is nil.
    (VZErrorDomain 2)`.
  - `initWithConfiguration:queue:` on a private serial dispatch queue.
  - `startWithCompletionHandler:` with an `objc.NewBlock`. The block ran on a
    GCD worker thread that Go did not make: purego's fakecgo handles it.
  - `connectToPort:completionHandler:` (dup the fd, close the
    VZVirtioSocketConnection, wrap the dup in a nonblocking `os.File`). An
    unused port answers `Connection reset by peer (NSPOSIXErrorDomain 54)`.
  - A delegate class made with `objc.RegisterClass` and the
    VZVirtualMachineDelegate protocol: `guestDidStopVirtualMachine:` fires.
  - `stopWithCompletionHandler:`.
  - No run loop on the main thread.
- Load: 200 concurrent vsock connect+ping in 30 ms. 64 MiB through one
  connection in 0.65–0.75 s.
- Inside: Rosetta registered with flags `OCF`, `chroot … uname -m` is `x86_64`,
  `/sys/bus/virtio/devices` shows the block device (0x0002), and DHCP on NAT
  works.
- **SIGKILL of the helper ends the VM**: the VZ XPC service
  (`com.apple.Virtualization.VirtualMachine`, ~220 MB RSS) was gone a second
  later.
- **Didn't work**: `requestStopWithError:` returns YES but busybox init
  ignores the power-button event. A graceful stop is therefore an agent op
  (§4.3). A forced stop does not call the delegate (as documented), so the
  helper learns of it from the completion handler.

**buildx node routing** (real buildx 0.39.1, temp `BUILDX_CONFIG` and
`DOCKER_CONFIG`, two buildkitd v0.34.2 in the prototype VM: n0 with worker
platform `linux/arm64` only, like the gvx sandbox's, and n1 with
`linux/amd64,linux/arm64`, like the VM's; the node is read from
`--metadata-file`'s `buildx.build.ref` = builder/node/ref):

| n1 appended with | no `--platform` | `linux/arm64` | `linux/amd64` | `linux/amd64,linux/arm64` |
|---|---|---|---|---|
| `--platform linux/amd64` (**chosen**) | n0 | n0 | n1 | split: `-o type=local` gives `linux_amd64/` (x86_64) and `linux_arm64/` (aarch64); `type=oci` and `type=tar` fail |
| no `--platform` | n0 | n0 | n1 | split, same |
| `--platform linux/amd64,linux/arm64` | n0 | **n1** | n1 | n1 alone (oci works) |

Explicit platforms on a node make buildx prefer it, even for
`--platform linux/arm64`. So the VM's node is appended with
`--platform linux/amd64` only. It never takes arm64 work, even though its
worker can do arm64. buildx 0.39.1 answers `ERROR: failed to build: oci for
multi-node builds currently not supported` (and the same for `tar`). A split
multi-platform build can be exported with `type=local`, pushed
(`type=image,push=true`; buildx merges the manifests), or loaded per
platform. See §14, decision 1.

- `docker buildx create --append --name gvx-desktop --node N --platform
  linux/amd64 --driver-opt default-load=true unix://…` adds the node, and
  appending again updates it. `docker buildx create --leave --name gvx-desktop
  --node N` removes that node alone.
- **`docker buildx ls` and `docker buildx inspect` (even without
  `--bootstrap`) open a gRPC connection to every node.** So a probe would
  boot the VM through the lazy socket. Hence the probe idle rule (§8).

**rungv keeps each architecture's images apart.** `pull`, `load`, `images`,
`inspect`, `tag`, `rmi` and `save` take `-arch amd64|arm64` (default arm64):

- `rungv load` of an amd64 archive without `-arch` fails: `oci: image targets
  architecture "amd64", want arm64`. With `-arch amd64` it loads.
- `images`/`inspect` without `-arch` do not see amd64 images. With `-arch
  amd64`, inspect says `"Architecture": "amd64"`.
- Both live in the same cache directory (`image-*.json`, `tag-*.json`, the
  EROFS layers).

**Signing in ggui.** `machosign.SignBundle` signs ad hoc, on any host. It
re-signs every nested Mach-O (`Contents/MacOS/*`, `Helpers/…`) **without
entitlements** and only the main executable with `macos.entitlements`. ggui
cannot build a second program into the bundle either. So a separate helper
binary in the bundle would lose its entitlement. The main executable keeps its
own, in dev (`ggui dev`, ad hoc) and in release (ad hoc or Developer ID).

## 3. Process model

```
 docker buildx build --platform linux/amd64
        │ gRPC over unix:// (buildx remote driver, node gvx-desktop-amd64)
        ▼
 ┌─ GVX Desktop (app process) ──────────────────────────────────────────┐
 │ engine: lazy front socket <socketDir>/buildkit-amd64.sock (0600)     │
 │   first connection → assets (vmassets.Resolve) → helper (vmclient)   │
 │   → start VM → dial "buildkitd" → pipe bytes; idle timers            │
 └──────────────┬───────────────────────────────────────────────────────┘
   stdin pipe = lease (EOF → stop VM, exit)   control: <dir>/helper.sock (0600, vmproto)
 ┌─ VM helper (same executable, "__vm-helper serve") ───────────────────┐
 │ vmhelper: control server, VM manager, guestproto client, forwarder   │
 │ internal/vz: VZVirtualMachine on its own dispatch queue (pure Go)    │
 └──────────────┬───────────────────────────────────────────────────────┘
   XPC (Apple's)                  virtio-vsock port 1024 (guestproto)
 ┌─ com.apple.Virtualization.VirtualMachine (Apple's XPC service) ──────┐
 │ Linux 6.18 (Alpine linux-virt), initramfs → tmpfs root               │
 │  agent (PID 1's child): hello/exec/dial/disk-mount/services/shutdown │
 │  buildkitd (from /mnt/assets, state on /dev/vda → /var/lib/gvx)      │
 │  Rosetta: /mnt/rosetta + binfmt_misc (flags CF)                      │
 └──────────────────────────────────────────────────────────────────────┘
```

- **Why a separate process** (the user's decision 2): a VZ crash or hang
  cannot take the app with it. The entitlement-bearing VM code stays out of
  the app's address space at run time. And the VM's life is tied to one small
  process that the app can kill.
- **The helper is the app's own executable** in helper mode. `main.go` checks
  `os.Args[1] == vmhelper.Arg` (`__vm-helper`) before anything else and runs
  `vmhelper.Main`. Nothing of the app runs: no ggui, no store. ggui's darwin
  `init`s only dlopen libSystem. The bundle needs no second binary, the
  entitlement rides on the main executable (§11), and the helper cross-builds
  with the app. `cmd/gvx-vmhelper` is the same code as a standalone program,
  for development and the engine's tests.
- **Lifetimes.** The app starts the helper on the first build (§8), with stdin
  a pipe from the app (the lease). EOF on stdin (app quit, crash, `Close`)
  makes the helper stop the VM gracefully (agent shutdown, then force after
  15 s) and exit. A helper's death ends the VM (verified with SIGKILL). The
  engine watches the helper process (`Client.Done`), so a crash makes the next
  connection start a new helper.
- **Logs**: the helper's stderr goes to `Launcher.Log` (the engine's `logf`).
  The guest console goes to `<dir>/console.log`, capped at 1 MiB and rotated
  once at each start.

## 4. Protocols

Both use the same framing: one JSON request line, one JSON response line, then
the op's own bytes (`vmproto.ReadLine/WriteLine`, at most 1 MiB a line).
Requests carry `v`. An unknown version or op is answered `{"ok":false,
"code":"bad-version"|"unknown-op"}`, never a hang-up, so either side can be
newer. New ops and fields are added, never changed.

### 4.1 App ↔ helper: `internal/vmproto` (frozen)

The control socket is `<engine Dir>/amd64/helper.sock`, mode 0600 (in the
app, `~/<data>/engine/amd64/`). When that path is too long, the engine uses a
short temp directory instead, as builder.go does.

| op | request | response | then |
|---|---|---|---|
| `hello` | — | `Hello{protocol, minProtocol, helper, pid, features}` | close |
| `status` | — | `Status{state, err, since, spec, services, console, vmPid}` | close |
| `start` | `start.spec: VMSpec` | `Status` once the VM and every service are ready, or failed | close |
| `stop` | `stop: {timeout (ms), force}` | `Status` once stopped | close |
| `dial` | `dial: {service}` or `{network, addr}` | ok | raw stream both ways |
| `events` | — | ok | `Event{time, status}` lines, current one first |
| `shutdown` | `stop?` | ok, after the VM stopped | helper exits |

`VMSpec` (the app decides it): kernel, initrd and assets paths; cpus,
memoryMiB; `disk {path, size}` (made sparse when missing); rosetta; network;
`services: ["buildkitd"]`; extra commandLine. A `start` while starting waits
for it. A `start` with a different spec answers `busy`. The helper writes a
`Ready{ready, pid, protocol, socket}` line on stdout once it listens, so the
app never races the socket. Codes: `bad-version`, `unknown-op`,
`bad-request`, `not-running`, `busy`, `unsupported`, `failed`, `no-service`,
`guest-error`, `internal`, `shutting-down`.

Subcommands besides `serve`: `check` prints a `CheckResult{appleSilicon,
macOS, virtualization, rosetta, err}` (Rosetta from
`VZLinuxRosettaDirectoryShare.availability`). `install-rosetta` calls
`installRosettaWithCompletionHandler:` (macOS shows its own dialog) and prints
the result. The app runs both as short-lived helper processes, so the app
process never touches VZ.

### 4.2 Helper ↔ agent: `internal/guestproto` (frozen)

vsock port 1024. Shared names: tags `assets` and `rosetta` (mounted at
`/mnt/<tag>`), kernel command line `gvx.assets=assets gvx.rosetta=rosetta
gvx.net=1`, data disk `/dev/vda` → `/var/lib/gvx` (ext4, label `gvx-data`),
service `buildkitd` on `/run/buildkit/buildkitd.sock`.

| op | request | response | then |
|---|---|---|---|
| `hello` | — | `Hello{protocol, agent, kernel, machine, rosetta, uptime}` | close |
| `disk-mount` | `{format}` | `DiskStatus{formatted, size, free, fsType}` | close |
| `service-start` | `{name, timeoutMs}` | `[ServiceStatus]` once ready (the socket accepts) | close |
| `service-stop` | `{name, timeoutMs}` | `[ServiceStatus]` | close |
| `service-status` | — | `[ServiceStatus]` | close |
| `exec` | `{argv, env, dir, stdin}` | ok | frames: host→ `Stdin`/`StdinEOF`, agent→ `Stdout`/`Stderr`/`Exit{code, elapsedNs, error}` |
| `dial` | `{service}` or `{network, addr}` | ok | raw stream |
| `shutdown` | — | ok | agent stops services, syncs, unmounts, `poweroff -f` |

VZ vsock has no half close (prototype problem 8), so nothing depends on one.
The end of exec input is a frame, and a dial ends when either side closes.
gRPC does not half-close. The agent knows what a service name runs (the
guest's business, workstream B). The host only names it.

### 4.3 Start and stop sequences

Start (helper): validate the spec → make the disk if missing (sparse
`Truncate`) → `vz.New` + `Start` → poll `connect(1024)` + `hello` every 5 ms
up to 10 s → `disk-mount {format: true}` → for each service `service-start`
→ state Running.

Graceful stop: `service-stop` each → `shutdown` → wait for the delegate's
`guestDidStop` up to the timeout → else `Stop` (force). A graceful stop
matters: ext4 on the disk. Forced stops leave a journal to replay, which ext4
does.

## 5. The pure-Go VZ layer: `internal/vz` (workstream A)

From the spike, made into a package:

- **Runtime** (`objc_darwin.go`): dlopen Foundation and Virtualization (and
  libSystem/libobjc). Register `dispatch_queue_create`, `dispatch_sync_f`,
  `objc_autoreleasePoolPush/Pop`. Helpers: `NSString` (alloc +
  `initWithUTF8String:`), `NSURL` (`initFileURLWithPath:`), `NSArray`
  (`initWithObjects:count:`), `NSError` → Go error (localizedDescription,
  domain, code), C strings.
- **Queue hops**: one `purego.NewCallback` trampoline for the whole process.
  The Go func rides in the context pointer as a handle (a `sync.Map` of
  funcs), so the 2000-callback table of purego is never exhausted.
  `onQueue(q, f)` pushes an autorelease pool, runs `f`, pops it.
  `dispatch_sync_f` usually runs the work on the calling thread.
- **Blocks**: `objc.NewBlock(func(objc.Block, …))` caches one callback per Go
  func *type*, not per block. Release our reference right after passing a
  block (VZ copies it). A block body only sends on a buffered channel: it
  never blocks a VZ thread, never calls back into VZ.
- **Threading rules**: configuration objects may be built on any goroutine
  (inside a pool). The VZVirtualMachine is made with
  `initWithConfiguration:queue:` on its own serial queue. Every message to it
  (`start`, `stop`, `state`, `socketDevices`, `connectToPort:`) goes through
  `onQueue`. Completion handlers and the delegate run on that queue, on
  threads Go did not make (fakecgo). There is no main-thread run loop, and
  the helper needs no locked OS thread.
- **Delegate**: one class (`GVXVMDelegate`, NSObject +
  VZVirtualMachineDelegate) registered once per process. An instance maps to
  a Go VM by its pointer. `guestDidStopVirtualMachine:` and
  `virtualMachine:didStopWithError:` close `Done`.
- **vsock**: in the completion handler, `dup(fileDescriptor)`, `close` the
  connection object, `CloseOnExec`, `SetNonblock`, `os.NewFile`, wrapped in a
  `net.Conn` (`vsockConn`: Local/RemoteAddr of a `vsockAddr`, deadlines from
  the os.File poller). Connect errors are NSErrors.
- **API**: `Config{Kernel, Initrd, CommandLine, CPUs, MemoryBytes, Console,
  Shares, Rosetta, RosettaTag, Disks, NAT, Balloon}`, `New(Config) (*VM,
  error)` (validates), `(*VM).Start(ctx)`, `Stop(ctx)`, `State()`, `Done()`,
  `Err()`, `Connect(ctx, port) (net.Conn, error)`, `RosettaAvailability()`,
  `InstallRosetta(ctx)`. `vm_other.go` keeps the stubs (`ErrUnsupported`) for
  other systems. `go vet` runs for windows and linux too.
- **Memory management**: objects we alloc+init we own (+1). Release the
  configuration after the VM is made. Release the VM after it stopped.
  Objects from class conveniences are autoreleased into the pool of the
  current hop.

## 6. VM assets

### 6.1 What is in them

| file | from | notes |
|---|---|---|
| `Image` | Alpine `linux-virt` 6.18.64-r0 (aarch64) | EFI zboot unwrapped to a bare Image (prototype problem 1) |
| `initramfs.gz` | busybox-static 1.37.0-r31, e2fsprogs (`mke2fs`, `e2fsck`, `blkid`) with the musl and libs it needs, the agent, `guest/rootfs/*`, modules | modules gunzipped, in `modules.dep` order: the prototype's 15 plus `virtio_blk`, `ext4` (+ `jbd2`, `mbcache`, `crc16`, crc32c as the kernel needs) |
| `buildkit/bin/{buildkitd,buildkit-runc,buildctl}` | `moby/buildkit:v0.34.2` linux/arm64, by manifest digest | static. Shared read-only over virtiofs, never in RAM (~120 MB) |
| `buildkit/certs/ca-certificates.crt` | the same image | for pulls and pushes |
| `buildkit/buildkitd.toml` | `guest/` | GC: keep ≤ 20 GB on the data disk, worker platforms amd64+arm64 |
| `manifest.json` | mkassets | `Manifest{version, components[{name, version, source, sha256}], files{path: sha256}}` |
| `SOURCES` | mkassets | every source URL/digest, for the GPL's offer of source |

The guest's `/init` copies the initramfs to a tmpfs and `switch_root`s
(runc pivots). Its `rc` loads the modules, mounts the shares and registers
Rosetta (`:rosetta:M::…:/mnt/rosetta/rosetta:CF`). Then busybox init starts
the agent. The agent mounts the disk and runs buildkitd when the helper asks.
buildkitd gets `--oci-worker-platform linux/amd64 --oci-worker-platform
linux/arm64` (its probe chroots without /proc, prototype problem 5).
`SSL_CERT_FILE` points at the bundle. `/proc` is mounted wherever Rosetta
runs.

### 6.2 Reproducible build: `guest/mkassets` (workstream B)

`go run ./guest/mkassets -out DIR -archive FILE [-cache DIR]` is a Go
program, so it runs the same on Linux CI and macOS: no python, no bsdtar, no
openssl.

- Every input is pinned in `guest/mkassets/pins.go` by URL and SHA-256:
  Alpine `.apk` files (not the APKINDEX, which moves), and the buildkit image
  by index and arm64 manifest digest, then each layer by digest. Seeds from
  the prototype run: linux-virt-6.18.64-r0.apk
  `cf171f51ab44d637a7758dd5a2f6a4ecd7ddbea18d61b9e37a6d39357d2fb1d5` and
  busybox-static-1.37.0-r31.apk
  `965777e06b94bf11981d5f4ecdfcd577879f4b0dda544294d1dd3f72b217bc75`.
- Alpine drops old package versions from its branch mirrors. A pin that
  vanished fails with "bump the pins". The release archive is the durable
  artifact.
- The agent is built with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build
  -trimpath -buildvcs=false -ldflags '-s -w -buildid='`.
- Determinism: cpio entries sorted, uid/gid 0, mtime 0. gzip with no name and
  mtime 0 at a fixed level. The tar archive sorted, with fixed modes, owner 0
  and mtime 0. Two runs from two empty caches must give the same archive
  SHA-256 (acceptance test).
- It writes `<archive>.sha256` and prints the `ArchiveSHA256`/`ArchiveSize`
  lines to paste into `internal/vmassets/vmassets.go`.

### 6.3 Distribution: GitHub release, then the user's cache

- A release `vm-assets-v1` of this repository (`vmassets.ReleaseTag`) carries
  `gvx-vm-assets-v1-arm64.tar.gz`, about 73 MB (73102954 bytes in B's reproducible build). A new kernel or buildkit means
  a new `Version`.
- `.github/workflows/vm-assets.yml` (B) builds it on ubuntu on a push of a
  `vm-assets-v*` tag and attaches archive, `.sha256` and `SOURCES`.
  **Publishing is the user's step** (decision 3).
- `vmassets.Resolve` (C):
  1. With an override (`Options.Override` or `$GVX_VM_ASSETS`), use that
     directory, verified against its own manifest.
  2. Else the cache, `<UserCacheDir>/gvx-desktop/vm-assets/<Version>/`,
     re-verified on every Resolve (cheap: ~170 MB hashed once per process;
     the engine memoizes per open).
  3. Else download `ReleaseURL` with `ghrelease.Download` (size cap
     `ArchiveSize`), check `ArchiveSHA256`, unpack into a temp dir beside
     the cache (refusing absolute paths, `..`, links and devices), verify,
     then rename into place. A lock file makes two resolves end with one
     directory.
  4. With `ArchiveSHA256` empty and no override: `ErrNotPublished`, shown
     in Settings.

## 7. The persistent disk

- `<engine Dir>/amd64/disk.img`: a raw image, sparse, 64 GiB apparent (APFS
  allocates only what is written), attached read-write as virtio-blk.
- On first boot the agent finds no file system (`blkid`) and runs `mke2fs -t
  ext4 -L gvx-data -E lazy_itable_init=1,lazy_journal_init=1`. Each boot it
  runs `e2fsck -p`, then mounts at `/var/lib/gvx` (buildkitd's root is
  `/var/lib/gvx/buildkit`).
- It is outside rungv's cache, so it is out of reach of every prune and remove
  path (engine volumes, rungv `volume prune`, image prune, the UI's volume
  pruning). The engine's `ResetAmd64Disk` is the only way to delete it: it
  stops the VM first.
- buildkit's own GC (buildkitd.toml) keeps the cache under 20 GB.
  `docker buildx prune --builder gvx-desktop` reaches it too.
- Settings shows its allocated size (`Amd64State.DiskBytes`) and "Reset amd64
  build cache".

## 8. Lifecycle

All of this is engine-side, in `amd64.go` (C), following builder.go's
patterns and lock order.

- **Open / SetAmd64**: with a config, bind `<socketDir>/buildkit-amd64.sock`
  (0600, `listenBuilderUnix`'s safe-replace rules). Nothing else happens yet.
- **Lazy start** on the first connection, single-flight:
  1. `Assets` (may download; progress goes to `Amd64State`).
  2. `Launcher.Launch` with `Dir = <Dir>/amd64`.
  3. `Start(spec)` with defaults: CPUs = max(2, NumCPU/2), 4096 MiB, the disk,
     Rosetta and NAT on, services `["buildkitd"]`.
  4. For each connection, `Dial({service: "buildkitd"})` and
     `pipeHalfClose`.

  Expected: ~0.4 s boot plus ~1 s for buildkitd. A first download adds its own
  time.
- **Idle stop**: with zero connections, a timer of `IdleTimeout` (10 min)
  stops the VM. When everything since the start moved less than 64 KiB in
  total (`amd64ProbeBytes`), the timer is `ProbeIdleTimeout` (30 s) instead,
  so a `buildx ls`/`inspect` probe costs half a minute of VM, not ten. The
  stop is `Client.Close(15 s)`: graceful, then force, and the helper exits.
  Memory goes back to macOS with the VM process. The VM process grows to
  ~600 MiB during builds and does not shrink.
- **Balloon**: configured but not driven in v1. Later, the helper could
  inflate it after a minute idle (feature `"balloon"` in `Hello`).
- **Crash**: the helper exits unexpectedly (`Done`), or the VM fails
  (`Status.State == failed`). The state goes idle with `Err`, connections
  close, and the next connection starts again. A start failure is reported to
  the connecting client by closing it, and to Settings by `Err`.
- **Quit / engine close / SetAmd64(nil) / sandbox engine off**: close the
  front socket and its connections, then `Client.Close(timeout)`. The app
  quitting without that still ends everything (the lease).
- **Restart persistence**: buildkit's state is on the disk. A build after an
  idle stop hits the cache (acceptance).

## 9. buildx routing (C, in `internal/ui/model_buildx.go`)

- The `gvx-desktop` builder (node 0: the gvx sandbox's buildkitd, as today)
  gets node `gvx-desktop-amd64`: `docker buildx create --append --name
  gvx-desktop --node gvx-desktop-amd64 --platform linux/amd64 --driver-opt
  default-load=true unix://<Amd64Socket>`.
- Turning the setting off, or the sandbox engine off: `docker buildx create
  --leave --name gvx-desktop --node gvx-desktop-amd64`. Only that node goes.
  When `configureBuildx` has to make the builder anew (stale endpoint), it
  appends the node again if the setting is on.
- `syncAmd64Node(on)` is the seam D calls. It runs async like
  `syncSandboxBuilder` and reports `BuildxStatus.Amd64Node`. It reads the
  node list from `docker buildx inspect gvx-desktop`, which connects: with
  the engine open, that probe boots the VM for 30 s. To avoid it, run the
  append/leave before `SetAmd64` binds the socket where possible. The
  inspect output (`Nodes`, `Name`, `Endpoint`, `Platforms`) parses as today.
- Routing is buildx's: `--platform linux/amd64` → VM, native or
  `linux/arm64` → gvx node, both → split (§2).

## 10. Images of another architecture (C)

- **Runner**: `ImageArchRunner` (`arch.go`, frozen) is an optional interface
  of the Runner. It has the `…Arch(ctx, arch, …)` variants of images,
  inspect, rmi, tag, load, save and prune. `rungvRunner` implements it with
  `-arch` (new `rungv.Client` methods in `internal/rungv/engine.go`, or an
  `Arch` field the command builder adds). The fake implements it in
  `fake_arch_test.go`.
- **Load** (`POST /images/load`, which buildx's `default-load` uses):
  1. Spool the body to `Config.Dir/tmp`.
  2. Read the archive's architecture: docker-archive `manifest.json` → config
     `architecture`, or OCI `index.json` → manifest → config. A multi-arch
     OCI archive loads each architecture it has.
  3. `LoadImagesArch` with it (host arch → the plain path).
- **List** (`/images/json`): the host's rows plus the foreign namespace's.
  Every row carries `Manifests: [{ID, Available: true, Kind: "image",
  ImageData: {Platform: {architecture, os}}}]` (API 1.47+), so clients and the
  app's Images page see the architecture. Check the real `docker images` 29.10
  renders it.
- **Inspect/tag/rmi/save** by name try the host namespace first, then the
  foreign one. An ID resolves in whichever has it. The same tag can exist in
  both, and lists as two rows. `image prune` (dangling, and `-a`) covers both,
  with the same keep rules.
- **Run**: `containerCreate` calls `refuseForeignImage(ctx, ref, platform)`
  before `imageConfig` (already in the skeleton). It answers 400 with
  `foreignRunMessage` ("amd64 images can be built and pushed, not run, yet:
  gvxd-amd64:1 is linux/amd64 and GVX Desktop runs linux/arm64 containers")
  when the ref is cached only as amd64, or `platform` is `linux/amd64`. Never
  404, which would make the CLI pull.
- **Pull** `--platform linux/amd64` stores into the amd64 namespace (`pull
  -arch amd64 -platform linux/amd64`), for inspect, tag and save. Run is
  refused as above.
- **Push** from buildkitd (`--push`): buildkit asks buildx for credentials
  over the build session, which rides the forwarded gRPC connection, so the
  user's `DOCKER_CONFIG` credentials are used and nothing is stored in the VM.
  Check: §13.

## 11. Signing and entitlements

The helper is the app's executable, so the entitlement goes on the main
executable:

- `assets/entitlements.plist` (`com.apple.security.virtualization`) is
  already in the skeleton. `ggui.toml` gets `[macos] entitlements =
  "assets/entitlements.plist"` (D).
- ggui signs the main executable with it ad hoc (`ggui dev`, a release
  without a certificate) or with Developer ID plus hardened runtime (a
  release with `GGUI_P12_*`; the virtualization entitlement needs no
  provisioning profile). `release/check-bundle.sh` checks the entitlement is
  in the bundle's signature (D).
- `cmd/gvx-vmhelper` (dev and tests) is signed `codesign --sign -
  --entitlements assets/entitlements.plist --force` by the test that builds it
  (C's harness, A's integration tests).
- **Not chosen**: (a) a second binary in `Contents/MacOS` or `Helpers` would
  need a ggui change: build an extra main package into the bundle and sign
  nested programs with their own entitlements (`SignBundle` signs them without
  any today). (b) Shipping it outside the bundle and signing it at run time
  like rungv needs `codesign` (Xcode CLT) on the user's Mac and a second
  build step. Both are more moving parts than one entitlement on one binary.
  See §14, decision 2.

## 12. Settings UI (D)

Settings → Docker → Experimental, under the sandbox engine switch:

- Switch **"amd64 builds with Rosetta"**. It is disabled unless the sandbox
  engine is on, with the caption "Needs the sandbox engine above". Text:
  builds for `--platform linux/amd64` run in a small Linux VM with Rosetta
  for Linux; the images can be loaded and pushed, not run yet; the VM starts
  on the first amd64 build and stops when idle.
- **Requirements**, checked with `<exe> __vm-helper check` when the section
  shows:
  - Apple silicon, else the row says the feature is not available.
  - macOS 13+.
  - Rosetta installed, else an **Install Rosetta** button (runs
    `install-rosetta`; macOS shows its dialog) and the fallback text
    `softwareupdate --install-rosetta`.

  The switch cannot be turned on while one fails.
- **Status line**: amd64 builder idle / starting / running / stopping; a
  download progress line ("Downloading the VM, 23 of 73 MB"); errors
  (`Amd64State.Err`, `ErrNotPublished`); the disk's size; "Reset amd64 build
  cache" with a confirmation. The buildx line: "docker buildx sends linux/amd64
  builds to the amd64 node" when `BuildxStatus.Amd64Node`.
- **Model**: D builds `Amd64Config{Launcher: {Path: os.Executable(), Args:
  [vmhelper.Arg, vmhelper.CmdServe], Log: …}, Assets: vmassets.Resolve with
  the default cache dir}`.
  - At start with both settings on, and on toggle: `engine.SetAmd64(cfg)` or
    `nil`, then `syncAmd64Node(on)`.
  - Turning the sandbox engine off turns the node off too; the setting stays.
  - Quit is covered by `Engine.Close`.
  - An `Amd64State` signal, refreshed on `Changed`.
- **Images page**: an "amd64" badge on foreign rows (from `Manifests`; the
  dockerapi parse is D's). Run is not offered for them.
- **i18n**: every string through `tr` in all six catalogs; `ggui i18n
  check` has no errors.
- **Render shot**: `settings-amd64-builder` (en and ko).

## 13. Tests and the real verification plan

Unit tests run under `go test -short` without a VM:

- `vmproto`/`guestproto` round trips.
- The helper's server against a fake VM backend.
- vmclient against an in-process helper.
- `vmassets` with a fake HTTP server and a small archive (bad hash, traversal,
  concurrent resolve).
- mkassets' cpio/tar/gzip determinism on fixtures.
- The engine's amd64 front socket with a fake launcher: single-flight, idle
  and probe idle, crash restart, close.
- Foreign images over the fake: load arch detection, list rows, inspect order,
  the create refusal not being 404.
- model_buildx argument lists with a fake CLI runner.
- UI switch gating.

Real checks are opt-in, never under `-short`, with temp dirs only:

1. **A**: `CGO_ENABLED=0 go build ./cmd/gvx-vmhelper` and `GOOS=darwin
   GOARCH=arm64 CGO_ENABLED=0` give identical bytes, and `go version -m`
   says `CGO_ENABLED=0`. Then, with `GVX_VZ_TEST=1 GVX_VM_ASSETS=<B's dir>`:
   - the helper boots; `check` = installed;
   - a guest exec of `chroot`'d x86_64 `uname -m` = `x86_64` (the
     integration test shares a minirootfs);
   - dial buildkitd; graceful and forced stop;
   - stdin close → exit; SIGKILL → the VM process gone.
2. **B**: two `mkassets` runs from empty caches give the same archive
   SHA-256. The assets boot under A's helper (until it lands, under
   `experiments/vzpurego`): the disk is formatted on the first boot and kept on the second,
   buildkitd answers `buildctl debug workers` with linux/amd64,linux/arm64,
   and `shutdown` leaves a clean file system.
3. **C** (engine harness, `GVX_ENGINE_HARNESS=1`, its own
   `amd64_harness_test.go`): build and sign gvx-vmhelper, set
   `GVX_VM_ASSETS`, then `startHarness` + `SetAmd64` + the buildx node in the
   harness's temp `BUILDX_CONFIG`/`DOCKER_CONFIG`:
   - `docker buildx build --platform linux/amd64 --load -t gvxd-amd64:1 ctx`
     with `RUN uname -m > /arch` → `docker image inspect` says `amd64`.
     `/arch` = `x86_64`, from `FROM gvxd-amd64:1` + `RUN cat /arch` built for
     amd64, or from `-o type=local`.
   - A native build still goes to the gvx node (`--metadata-file`
     `buildx.build.ref` names node 0).
   - `--platform linux/amd64,linux/arm64 -o type=local,dest=…` gives
     `linux_amd64/arch` = x86_64 and `linux_arm64/arch` = aarch64. This
     replaces the requirement's `-o type=oci` check (§14, decision 1: not
     supported with the split builder, documented); the test asserts
     buildx's message for `type=oci` so a future buildx that lifts the
     limit is noticed.
   - Idle stop with short timeouts, then a second build: the VM restarts and
     the RUN step is `CACHED` (persistent disk).
   - `docker run gvxd-amd64:1` prints the clear message and exits non-zero
     without pulling.
   - Push auth: a stub registry on `0.0.0.0:18097` (net/http in the test,
     answering `/v2/` with a Basic challenge) and `--output
     type=image,name=192.168.64.1:18097/gvxd/x,push=true,registry.insecure=true`.
     The stub sees the credentials from the harness's `DOCKER_CONFIG`. Check
     the gateway address from the guest's DHCP lease.
4. **D**: render shots, UI tests. No normal launch of the app.
5. **Final end-to-end (C runs it, once A, B and D report done)**: all of 3,
   plus `buildx ls` showing both nodes, turning the node off (`--leave`)
   leaving node 0, and helper crash restart (kill the helper mid-idle; the
   next build works). Then the requirements' whole must-pass list on the tree.
   Then clean up: `/tmp/gvxd-*` dirs, any `gvx-vmhelper`/VZ processes of
   ours, sockets, the harness's buildx builders.

## 14. Decisions

Accepted by the coordinator, pending the user's review: 1 (a), 2, 3 and 4
as written below.


1. **Multi-platform OCI tarballs.** buildx 0.39.1 cannot export
   `type=oci`/`tar`/`docker` from a build split across nodes. `type=local`,
   `--push` and per-platform `--load` work. The requirement's
   `-o type=oci,dest=…` check therefore cannot pass with routing by node.
   Options:
   - (a) accept and document it (recommended for v1);
   - (b) also create a single-node builder `gvx-desktop-multiarch` on the
     same VM socket, with `--platform linux/amd64,linux/arm64`, for
     `--builder gvx-desktop-multiarch -o type=oci`; arm64 there runs natively
     in the VM;
   - (c) make the VM node take every multi-platform build, which buildx
     cannot express without also taking explicit arm64 builds (§2 table).
2. **The helper is the app's own executable** (`__vm-helper`), with the
   entitlement on the main binary. This needs no ggui change. A separate
   bundled binary would need ggui to build and sign nested programs with
   entitlements.
3. **Publishing the assets**: the release `vm-assets-v1` (a tag push and
   upload) is outward-facing and is the user's step. Until then
   `ArchiveSHA256` is empty, and development and the e2e use
   `GVX_VM_ASSETS`. The assets ship GPL code (Linux, busybox, e2fsprogs):
   `SOURCES` lists the exact Alpine sources. Whether to also attach the
   source tarballs to the release is the user's call.
4. Defaults: 4 GiB, half the CPUs, 64 GiB sparse disk, 10 min idle (30 s
   after probes only), no balloon driving.

## 15. Later: amd64 containers

Nothing in v1 assumes builds only:

- **Runtime in the guest**: a `containerd` (or plain `runc`) service the agent
  starts like buildkitd (`service-start {"name":"containerd"}`), with its state
  on the same disk. The asset set grows by those binaries (a new `Version`).
- **Images**: the amd64 images are already in rungv's amd64 namespace. To run
  one, the helper gets it into the guest (`rungv save -arch amd64` streamed
  through a `dial` to the guest's import endpoint, or a share). The
  `refuseForeignImage` seam becomes "route to the VM".
- **Engine**: the Runner grows a second backend. Containers whose image arch
  is foreign get a `Spec` the VM runner handles: start/stop/signal through new
  guestproto ops (`container-create/start/kill/wait`), stdio and attach over
  `dial` streams (exec-like frames, already half-close free), logs into the
  same logs store.
- **Ports**: `dial {network: "tcp", addr: "127.0.0.1:80"}` exists, so the
  engine publishes a host 127.0.0.1 port by accepting there and dialing
  through the helper. No guest networking changes.
- **Volumes and binds**: virtiofs shares added at start (VMSpec gets
  `shares`), or a later hot-plug feature. Note the host's case-insensitive
  file system (prototype "Not done").
- **Lifecycle**: a running container holds a reference like a build
  connection. The idle timer counts both.
- **Protocol**: new ops and `Hello.features` (`"containers"`); old helpers
  answer `unknown-op`, and the engine says "update".

## 16. Package layout and ownership

```
internal/vmproto/vmproto.go          app ↔ helper protocol                       [frozen]
internal/guestproto/guestproto.go    helper ↔ agent protocol, shared names       [frozen]
internal/vmhelper/api.go             Arg, subcommands, CheckResult, SocketName   [frozen]
internal/vmhelper/*.go (rest)        the helper: server, VM manager, forwarder   A
internal/vz/**                       pure-Go Virtualization.framework            A
internal/vmclient/vmclient.go        app-side client (API frozen, bodies A)      A
cmd/gvx-vmhelper/                    standalone helper                           A
guest/agent/                         the guest agent (linux)                     B
guest/rootfs/                        init, rc, inittab, udhcpc.sh (from the prototype; B adapts) B
guest/mkassets/                      reproducible asset build + pins.go          B
guest/buildkitd.toml                 buildkitd's config                          B (new)
.github/workflows/vm-assets.yml      release build of the assets                 B (new)
internal/vmassets/vmassets.go        assets API (frozen) + bodies                C
internal/engine/amd64_types.go       Amd64Config, Amd64State, constants          [frozen]
internal/engine/arch.go              ImageArchRunner, foreignRunMessage          [frozen]
internal/engine/amd64.go             front socket, helper, lifecycle (seams frozen) C
internal/engine/foreign_images.go    arch-aware images, run refusal (seam frozen)  C
internal/engine/images.go, runner_store.go, internal/rungv/engine.go             C
internal/ui/model_buildx.go (+_test) buildx node append/leave (syncAmd64Node)    C
internal/engine/engine.go, containers.go   hooks added by the skeleton            [frozen call sites]
internal/store/store.go              Settings.Amd64Builds (added)                D
main.go                              the __vm-helper dispatch (added)            D
internal/ui/* (but model_buildx.go), internal/dockerapi, render.go, ggui.toml,
assets/entitlements.plist, release/check-bundle.sh                               D
```

## 17. Workstreams

Every file has one owner. Frozen files are the coordinator's (the architect,
this session): to change one, send the coordinator the file, the exact diff
and why. A workstream creates new files only under its own paths and prefixes
(A: `internal/vz/`, `internal/vmhelper/`, `internal/vmclient/`,
`cmd/gvx-vmhelper/`; B: `guest/`, `.github/workflows/vm-assets.yml`; C:
`internal/vmassets/`, `internal/engine/amd64_*.go`, `foreign_*.go`,
`fake_arch_test.go`, `internal/rungv/engine*.go`; D: the rest of
`internal/ui/`, `internal/dockerapi/`). Never call another workstream's seam,
`emit` or `notify` holding your own mutex. Everyone runs the must-pass list
before reporting done. A red build caused by another workstream's file is
reported, not fixed.

| WS | Scope | Owned files | Provides | Needs |
|---|---|---|---|---|
| **A** VZ + helper | pure-Go VZ bindings (port the spike); the helper process (serve/check/install-rosetta, control server, VM manager, start/stop sequences, forwarder, logs); vmclient | `internal/vz/**`, `internal/vmhelper/**` but `api.go`, `internal/vmclient/**`, `cmd/gvx-vmhelper/**` | `vmclient.Launcher/Client`, `vmhelper.Main` | B's agent for integration; fake agent before |
| **B** guest | agent (guestproto server, services, disk, shutdown); rootfs scripts; mkassets (pins, kernel unwrap, modules incl. virtio_blk/ext4, e2fsprogs, cpio, buildkit by digest, manifest, deterministic archive); buildkitd.toml; vm-assets workflow | `guest/**`, `.github/workflows/vm-assets.yml` | an asset dir (`GVX_VM_ASSETS`) for A, C | A's `internal/vz` for booting (or `experiments/vzpurego`) |
| **C** engine + buildx + assets | amd64 front socket and lifecycle; foreign-arch images and the run refusal; rungv `-arch`; vmassets Resolve/Verify; buildx node append/leave; the amd64 harness; **the final end-to-end verification** | `internal/engine/amd64.go`, `foreign_images.go`, new `amd64_*`/`foreign_*` (+tests), `images.go`, `runner_store.go`, `fake_arch_test.go`, `internal/rungv/engine.go` (+test), `internal/vmassets/**` (bodies), `internal/ui/model_buildx.go` (+test) | `SetAmd64`, `Amd64Socket`, `Amd64State`, `ResetAmd64Disk`, `syncAmd64Node`, `vmassets.Resolve` | A's client, B's assets |
| **D** app | setting, requirements check and Rosetta install via the helper, Amd64Config wiring, entitlements in ggui.toml + bundle check, Settings UI, Images badge, i18n ×6, render shot | `internal/ui/*` but `model_buildx.go`, `internal/store/store.go`, `main.go`, `render.go`, `ggui.toml`, `assets/entitlements.plist`, `internal/dockerapi/*`, `release/check-bundle.sh` | — | C's seams (stubs exist); A's `check` |

### A — acceptance

- Unit (`-short`): the server with a fake VM: every op, bad version, unknown
  op, start single-flight, busy, stop under load, events.
- vmclient against it: Launch with a fake helper program (the test binary
  re-executed), Ready line, lease close → exit, stale socket replaced and a
  live one refused.
- Real: §13.1.
- Land `internal/vz` first (it is the spike, in package form) so that B can
  boot with it.

### B — acceptance

- `go test -short ./guest/...`: agent handlers over `net.Pipe` (keep them in
  portable files; only the vsock listener is linux), cpio/gzip/tar
  determinism.
- Real: §13.2. Report the asset dir path and its manifest to the coordinator.

### C — acceptance

- `go test -short ./internal/engine ./internal/vmassets ./internal/ui -run
  'Amd64|Foreign|Buildx|Assets'` green, and `-race ./internal/engine`.
- Real: §13.3, then the final e2e (§13.5).

### D — acceptance

- UI tests: the switch is disabled without the sandbox engine or with a failed
  check; toggling calls `SetAmd64` and `syncAmd64Node` (inject fakes).
- `TestCatalogsAreComplete`, `ggui i18n check` with no errors, the render
  shot in en and ko (look at the PNGs).
- `release/check-bundle.sh` verifies the entitlement (`ggui build --target
  macos` in a temp out dir, never launched).

### Order

A lands `internal/vz` and a fake-agent-tested helper. B lands the agent and an
asset dir. Then A's real integration runs on B's assets. C works against fakes
from the start and against the real helper once A reports. D works against
the stubs, which keep returning idle/off. C runs the final e2e when A, B and D
report done.
