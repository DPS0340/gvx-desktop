# Micro-VM 컨테이너 런타임 설계 (`amd64` + `arm64` 호환 모드, Labs)

Status: Accepted draft (decisions finalized; ready for implementation).

---

## 1. 개요 및 목표

GVX Desktop의 기본 Sandbox Engine([`docs/design/sandbox-engine.md`](sandbox-engine.md))은 각 컨테이너를 경량 gVisor 샌드박스(`rungv`)로 직접 실행합니다. 이 구조는 가볍고 빠르지만 두 가지 구조적 한계가 있습니다.

1. **`linux/amd64` 실행 불가**: gVisor는 `binfmt_misc`를 지원하지 않아 Apple Silicon에서 `linux/amd64` 이미지를 빌드·로드·푸시([`docs/design/amd64-builder.md`](amd64-builder.md))할 수는 있어도 `docker run`은 `refuseForeignImage`에서 400 오류로 거절됩니다.
2. **`linux/arm64` 일부 워크로드의 gVisor 호환성 제약**: `io_uring`, `iptables`/`nftables`, `FUSE`, 컨테이너 내부 `overlayfs`/중첩 네임스페이스, 특정 `privileged` 워크로드(Tailscale/WireGuard, DinD 등), `systemd`, 미구현 Linux 시스콜, 또는 시스콜 집약적 워크로드에서는 실제 Linux 커널이 필요합니다.

본 문서는 Apple의 `apple/container` / `apple/containerization`에서 검증된 **1 Container = 1 Micro-VM** 아키텍처를 `gvx-desktop-vm` (`vm-assets-v2`)과 `gvx-desktop`의 순수 Go 스택에 도입하여:
- **`linux/amd64` 컨테이너**는 Rosetta for Linux와 함께 Micro-VM에서 **자동 실행**하고,
- **`linux/arm64` 컨테이너**도 gVisor 호환성 이슈가 있을 때 표준 Docker **`--runtime=microvm`** (및 Compose `runtime: microvm`)으로 **실제 Linux 6.18 커널에서 네이티브 속도로 선택 실행**할 수 있게 하는 Labs 기능 설계를 정의합니다.

### 목표

1. **1 Container = 1 Micro-VM (Zero `containerd`, Zero `runc`)**: 컨테이너당 1개의 초경량 Virtualization.framework Linux VM을 기동하고, 게스트 에이전트(`gvx-agent`)가 직접 루트파일시스템과 PID 1을 구동합니다. 컨테이너 종료 시 VM이 즉시 종료되어 메모리가 macOS로 100% 반환됩니다.
2. **`arm64` 네이티브 호환 실행 + `amd64` 자동 Rosetta 실행**:
   - `linux/arm64` + `--runtime=microvm`: `VMSpec.Rosetta = false`로 Apple Silicon **네이티브 속도** + **실제 Linux 6.18 커널** 제공 (Rosetta 미설치 환경에서도 동작!).
   - `linux/amd64` (기본 또는 `--runtime=microvm`): `VMSpec.Rosetta = true`로 `binfmt_misc` (`CF`)를 통해 투명 실행.
   - `linux/arm64` 기본(`--runtime` 미지정 또는 `--runtime=gvx`): 기존대로 초경량 `rungv` gVisor 샌드박스 유지.
3. **제로-카피 EROFS 직접 마운트 (`vm-assets-v2`)**: `gvx-desktop-vm`을 `vm-assets-v2`(`erofs.ko` + `loop.ko` + `tun.ko` + `wireguard.ko`)로 먼저 업그레이드하여, `rungv`가 이미 캐시해 둔 `arm64`/`amd64` `.erofs` 레이어를 **별도 `ext4` 변환·복사·재다운로드 없이** Linux 커널 `erofs` + `overlayfs`로 직접 마운트합니다.
4. **macOS 13+ 및 순수 Go `CGO_ENABLED=0` 유지**: Swift 6 및 macOS 26(Tahoe)+ 전용인 `apple/container`에 직접 의존하지 않고, 기존 [`github.com/ironpark/gvx-desktop-vm`](https://github.com/ironpark/gvx-desktop-vm)의 `purego/objc` `vz` 바인딩을 그대로 확장합니다.
5. **단일 헬퍼(`__vm-helper`) & 단일 게스트 에이전트(`gvx-agent`)**: `gvx-desktop`에 별도 Linux 바이너리를 임베드하거나 `main.go`에 새 서브커맨드를 추가하지 않고, `gvx-desktop-vm`의 `vmhelper`/`vmproto`/`guestproto`/`guest/agent`를 하위 호환 확장(`Hello.Features: ["containers"]`)합니다.
6. **기존 Docker Engine API 및 Labs 기능과 투명한 통합**: `docker run/start/stop/kill/restart/rm/logs/attach/wait/exec/top/stats/pause/unpause/cp/diff/export/commit/inspect`, UI Containers/Terminal, Labs 로컬 HTTPS 프록시(`*.localhost`), 내장 MCP 서버가 수정 없이 Micro-VM 컨테이너(`arm64`·`amd64` 모두)를 동일하게 제어합니다.

### 비목표 (v1)

- Apple Silicon macOS(13+) 외 플랫폼(Intel Mac, Windows, Linux) 지원
- 호환 모드(`Settings.SandboxEngine == false`, `docker:dind`)에서의 지원
- `--network=host`, `--network=container:<id>`, GPU 패스스루, 체크포인트 스냅샷
- UDP 포트 퍼블리싱 (vsock TCP 스트림 우선 지원, UDP는 경고 반환)

---

## 2. 검증된 기반 사실 및 `apple/container` 참고 사항

### 2.1 `gvx-desktop-vm` 및 Alpine `linux-virt-6.18.64-r0` 검증 결과

현재 `gvx-desktop-vm v0.1.1`에 핀된 Alpine `linux-virt-6.18.64-r0.apk` 내부를 확인한 결과, **추가 패키지나 커널 소스 변경 없이** 필요한 모든 커널 모듈이 이미 존재합니다.

| 모듈 | `linux-virt-6.18.64-r0.apk` 내 경로 | 역할 |
| :--- | :--- | :--- |
| **`erofs.ko`** | `kernel/fs/erofs/erofs.ko.gz` | `rungv` 캐시의 `arm64`/`amd64` `.erofs` 레이어 직접 읽기 전용 마운트 |
| **`loop.ko`** | `kernel/drivers/block/loop.ko.gz` | 다수 레이어(>8개) 이미지의 `virtiofs` 백업 loop 디바이스 마운트 |
| **`tun.ko`** | `kernel/drivers/net/tun.ko.gz` | Compose 오버레이 네트워크(`/dev/net/tun`) 및 Tailscale 등 VPN 컨테이너 지원 |
| **`wireguard.ko`** | `kernel/drivers/net/wireguard/wireguard.ko.gz` | 커널 레벨 WireGuard 및 오버레이 가속 기반 |
| **`overlay.ko`** | `kernel/fs/overlayfs/overlay.ko.gz` | (v1 기포함) EROFS `lowerdir` + 영속 `ext4` `upperdir` 병합 |
| **`ext4.ko` / `virtio_blk.ko` / `virtiofs.ko` / `binfmt_misc.ko` / `vsock.ko` / `virtio_net.ko`** | (v1 기포함) | 영속 쓰기 레이어, 바인드 마운트, Rosetta(`CF`), 제어 vsock, NAT |

- **EROFS 호환성**: [`internal/imagefs/erofs.go`](../../internal/imagefs/erofs.go) 확인 결과, `rungv`가 생성하는 레이어 파일(`<hex>.<format>.erofs`)은 표준 Linux EROFS 포맷(매직 `0xe0f5e1e2`, `FlatPlain`/`FlatInline`/`ChunkBased`, `char(0,0)` whiteout, 인라인 `trusted.overlay.opaque=y` xattr)으로, `arm64`·`amd64` 이미지 모두 Linux 6.18 `erofs.ko` + `overlay.ko`에서 변환 없이 그대로 마운트됩니다.
- **Rosetta `binfmt_misc` (`CF` 플래그)**: `guest/rootfs/rc`가 `:rosetta:M::\x7fELF...:/mnt/rosetta/rosetta:CF` (`F` = `fix-binary`)로 등록하므로, `amd64` 컨테이너에서 `pivot_root` 이후에도 `x86_64` ELF가 투명하게 실행됩니다. `arm64` 컨테이너는 `VMSpec.Rosetta = false`로 부팅해 Rosetta 마운트/등록 자체를 생략합니다.
- **재현 빌드 & 라이선스**: `linux-virt-6.18.64-r0.apk` 핀 자체가 동일하므로 `guest/mkassets/pins.go`의 `requiredModules` 확장만으로 `vm-assets-v2`의 GPL 대응 소스 아카이브(`SOURCES`)가 그대로 유지되며, `gvx-desktop-vm/.github/workflows/vm-assets.yml`이 2회 빈 캐시 빌드로 바이트 단위 재현성을 자동 검증·배포합니다.

### 2.2 `apple/container` / `apple/containerization` 참고 사항

| 설계 영역 | `apple/container` 구현 | GVX Desktop 적용 방식 |
| :--- | :--- | :--- |
| **격리 단위** | 1 Container = 1 Virtualization.framework Micro-VM | **동일 채택** (컨테이너당 1 헬퍼 프로세스 + 1 VZ VM) |
| **게스트 에이전트** | `vminitd` (initfs 내장 정적 바이너리, vsock gRPC) | **`gvx-agent` 확장** (`initramfs.gz` 내장 정적 Go 바이너리, vsock `guestproto` JSON+프레임) |
| **기본 리소스** | 컨테이너당 **`4` CPUs, `1 GiB` RAM** (`--cpus`/`--memory` 및 `config.toml` `[container]` 전역 설정) | **동일 기본값 채택**: 기본 **`4` vCPUs** (`min(4, NumCPU)`) / **`1 GiB` (`1024 MiB`) RAM** + Labs 전역 기본값 설정 + Docker `-m`/`--cpus` 오버라이드 |
| **루트파일시스템** | 호스트에서 OCI 레이어를 `ext4`로 평탄화 후 `virtio-blk` 첨부 | **GVX 개선**: `rungv`가 이미 컴파일한 **EROFS 레이어(`arm64`·`amd64` 공용)를 제로-카피 직접 마운트** (`virtio-blk` / `loop`) + 영속 `ext4` `upperdir` `overlayfs` |
| **호스트 최소 버전** | macOS 26 (Tahoe)+, Swift 6 | **macOS 13+**, 순수 Go (`CGO_ENABLED=0`) |

---

## 3. 런타임 선택 및 라우팅 규칙 (`gvx` vs `microvm`)

Docker Engine 표준 `HostConfig.Runtime` (`--runtime=<name>`)과 `GET /info`의 `Runtimes` / `DefaultRuntime` 필드를 그대로 따릅니다.

### 3.1 등록 런타임 (`GET /info`)

- `DefaultRuntime`: `"gvx"` (유지)
- `Runtimes`:
  - `"gvx"`: `{"path": "rungv"}` (항상 등록)
  - `"microvm"`: `{"path": "gvx-microvm"}` (Labs Micro-VM 런타임이 활성화되었을 때 등록)

### 3.2 컨테이너 생성 시 런타임 결정 매트릭스

컨테이너 생성(`POST /containers/create`) 시 `HostConfig.Runtime` (보조로 `Labels["gvx.runtime"]`)과 이미지/요청 아키텍처(`platform`)의 조합으로 백엔드를 결정합니다.

| 요청 `Runtime` | 대상 아키텍처 | Labs Micro-VM ON | Labs Micro-VM OFF |
| :--- | :--- | :--- | :--- |
| `""` (기본) | `linux/arm64` | **`gvx`** (기존 gVisor 샌드박스) | **`gvx`** |
| `""` (기본) | `linux/amd64` | **`microvm`** (`Rosetta: true`, 미캐시 시 404 → CLI 자동 pull; Rosetta 미설치 시 400 안내) | **400** (Labs 활성화 안내) |
| `"microvm"` | `linux/arm64` | **`microvm`** (`Rosetta: false`, **네이티브 arm64 + 실제 Linux 6.18 커널**, Rosetta 불필요!) | **400** (`Unknown or disabled runtime "microvm"; enable Micro-VM runtime in Labs`) |
| `"microvm"` | `linux/amd64` | **`microvm`** (`Rosetta: true`, Rosetta 필요) | **400** |
| `"gvx"` | `linux/arm64` | **`gvx`** | **`gvx`** |
| `"gvx"` | `linux/amd64` | **400** (`linux/amd64 containers require the microvm runtime`) | **400** |
| 그 외 알 수 없는 이름 | any | **400** (`Unknown runtime specified <name>`) | **400** |

### 3.3 사용 예시

```bash
# 1. 기본 arm64 컨테이너: 기존처럼 초경량 gVisor 샌드박스(rungv)에서 실행
docker run --rm alpine uname -m                 # aarch64 (gvx)

# 2. arm64 호환성 이슈 워크로드: 실제 Linux 6.18 커널 Micro-VM에서 네이티브 실행
docker run --rm --runtime=microvm alpine uname -rm
# → 6.18.64-0-virt aarch64 (native speed, full Linux syscalls/io_uring/iptables/FUSE)

# 3. amd64 컨테이너: Micro-VM + Rosetta로 자동 실행
docker run --rm --platform linux/amd64 alpine uname -m
# → x86_64

# 4. Docker Compose에서 특정 서비스만 Micro-VM으로 지정
# services:
#   app:
#     image: myapp:latest          # 기본 gvx 샌드박스
#   db:
#     image: clickhouse/clickhouse-server
#     runtime: microvm             # 호환성 이슈 서비스만 실제 Linux 커널 Micro-VM
#   legacy:
#     image: legacy-x86:latest
#     platform: linux/amd64        # 자동 Micro-VM + Rosetta
```

---

## 4. 확정 아키텍처 결정 (Finalized Decisions)

1. **D1 — `vm-assets-v2` 선행 구축 (`gvx-desktop-vm v0.2.0`)**:
   - `gvx-desktop-vm`에서 먼저 `vm-assets-v2`(`erofs`, `loop`, `tun`, `wireguard` 모듈 + 확장 `gvx-agent`)와 `vmproto`/`guestproto`/`vmhelper`/`vmclient` v0.2.0을 완성·배포한 뒤 `gvx-desktop`의 `go.mod`를 `v0.2.0`으로 올려 연동합니다.
2. **D2 — 제로-카피 하이브리드 EROFS 마운트 (`arm64` & `amd64` 공용)**:
   - `rungv`가 캐시한 `.erofs` 레이어는 `arm64`와 `amd64`가 동일한 포맷과 `LayerCacheDir`에 저장되므로, `arm64` 이미지를 `gvx`로 받았던 상태에서도 `--runtime=microvm` 실행 시 **재다운로드나 변환 없이 0 ms로 즉시 마운트**됩니다.
   - **Fast path (레이어 ≤ 8개)**: `.erofs` 파일들을 `vz.Disk{Path: layerPath, ReadOnly: true}` (`/dev/vdb`..`/dev/vdi`)로 직접 첨부.
   - **Overflow path (레이어 > 8개, 최대 128개)**: `LayerCacheDir`을 읽기 전용 `virtiofs`(`tag="layers"`)로 공유(또는 APFS 하드링크 디렉토리) 후 `loop.ko` (`/dev/loopN`, `LO_FLAGS_READ_ONLY`) + `erofs` 마운트 → `pivot_root` 후 구 루트 `MNT_DETACH`로 완전 격리.
3. **D3 — 동일 카테고리 별도 토글 (Labs UI)**:
   - Labs 페이지(Apple Silicon macOS 전용)의 `Virtualization & cross-architecture` (또는 `Cross-architecture & micro-VMs`) 그룹 안에 기존 `amd64 builds with Rosetta`(`Settings.Amd64Builds`)와 나란히 **`Micro-VM container runtime`**(`Settings.MicroVMContainers`) 스위치를 독립 배치합니다.
   - 요구사항 검사(`Amd64Check`) 중 **Apple Silicon + macOS 13+ + Virtualization**만 충족하면 토글 ON 가능(`arm64` `--runtime=microvm` 즉시 사용 가능!). Rosetta가 미설치된 경우에는 카드 내에 `"Install Rosetta to also run linux/amd64 containers"` 안내 및 **Install Rosetta** 버튼을 노출합니다.
4. **D4 — `apple/container` 기준 기본 리소스 + 전역/컨테이너 오버라이드**:
   - 기본 `4` vCPUs (`min(4, runtime.NumCPU())`), `1 GiB` (`1024 MiB`) RAM, 메모리 풍선(`VZVirtioTraditionalMemoryBalloonDevice`) 활성.
   - `store.Settings`에 전역 기본값(`MicroVMCPUs`, `MicroVMMemoryMiB`)을 두고 Labs UI에서 조정 가능하며, `docker run --cpus` / `-m`이 컨테이너별로 우선 적용됩니다.
5. **D5 — `gvx-agent` + `__vm-helper` 단일화**:
   - 별도의 `__microvm` 헬퍼나 임베드된 `vminit` 바이너리 없이, 기존 `vmhelper.Arg` (`__vm-helper serve`)와 `gvx-agent` (vsock port 1024)를 그대로 확장합니다.
   - `amd64` 빌더 VM은 기존과 동일하게 `Services: ["buildkitd"]`로, 컨테이너 Micro-VM은 `Services: nil` + `ContainerStart` op으로 부팅하여 완전 하위 호환됩니다.

---

## 5. 프로세스 모델 및 데이터 흐름

```text
  docker run --runtime=microvm -p 8080:80 clickhouse    (arm64, Rosetta=false)
  docker run --platform linux/amd64 -it alpine sh       (amd64, Rosetta=true)
         │ Docker Engine HTTP API
         ▼
 ┌─ GVX Desktop (App Process) ────────────────────────────────────────────────┐
 │ internal/engine:                                                           │
 │   • containerCreate: runtime / platform 매트릭스 판별 → gvx 또는 microvm    │
 │   • startContainer: Runtime == "microvm" → microVMManager                  │
 │   • logs / attach / wait / exec / top / stats / cp / LocalProxy / MCP      │
 │   • 호스트 TCP 포트 포워더: net.Listen → vmclient.Dial(tcp, 127.0.0.1:80)   │
 └──────────────────────────┬─────────────────────────────────────────────────┘
   stdin pipe (lease)       │ control Unix socket (<tmp>/gvxd-c-<short>/helper.sock)
                            ▼
 ┌─ VM Helper (<exe> __vm-helper serve -dir <sockDir>)  [컨테이너당 1 프로세스] ─┐
 │ gvx-desktop-vm/vmhelper:                                                   │
 │   • vmproto 서버 (features: ["containers"])                                │
 │   • pure-Go vz.VM (Apple Virtualization.framework, private dispatch queue) │
 └──────────────────────────┬─────────────────────────────────────────────────┘
   Apple XPC                │ virtio-vsock port 1024 (guestproto, host CID=2)
                            ▼
 ┌─ com.apple.Virtualization.VirtualMachine  [컨테이너당 1 Micro-VM] ─────────┐
 │ Linux 6.18.64 (aarch64), initramfs (vm-assets-v2) → tmpfs root             │
 │  • /mnt/rosetta + binfmt_misc (flags CF)   ← amd64 컨테이너일 때만 활성     │
 │  • /dev/vda        (virtio-blk rw: <cid>.ext4 → /var/lib/gvx/{upper,work}) │
 │  • /dev/vdb..vdi   (virtio-blk ro: rungv .erofs layers, ≤8 layers fastpath)│
 │    └ 또는 /mnt/layers (virtiofs ro) + /dev/loop0..N (erofs, >8 layers)      │
 │  • /dev/vdj..      (virtio-blk: rungv named/anon ext4 volumes)             │
 │  • /mnt/b0..bN     (virtiofs: host bind mounts via gvx.shares)             │
 │  • gvx-agent (vsock:1024, VMADDR_CID_HOST=2 only)                          │
 │      └─ Container PID 1 (NEWNS|NEWPID|NEWUTS|NEWIPC, pivot_root /mnt/rootfs│
 │            overlayfs[lower=erofs, upper=ext4]                              │
 │            arm64: 네이티브 Linux 6.18 실행 / amd64: Rosetta 투명 번역)       │
 └────────────────────────────────────────────────────────────────────────────┘
```

### 수명 주기 시퀀스

1. **시작 (`startContainer`)**
   1. `vmassets.Resolve`로 `vm-assets-v2` 경로 확보 (캐시 메모이즈; 최초 1회 다운로드 시 진행률을 Labs UI `MicroVMState`에 반영).
   2. `imagefs.ResolveGVXLayers(cacheDir, imageID)`로 해당 이미지(`arm64` 또는 `amd64`)의 EROFS 레이어 경로 목록(하단→상단)을 획득.
   3. `<exe> __vm-helper serve -dir <shortSockDir>` 실행 → `Ready` 수신.
   4. `vmclient.Start(VMSpec{...})` 호출:
      - `Rosetta: (c.Platform == "linux/amd64")` — `arm64` 컨테이너는 Rosetta 미첨부!
      - `Kernel`, `Initrd`, `CPUs`, `MemoryMiB`, `Network: (NetworkMode != "none")`, `Disk: <engineDir>/microvm/rootfs/<cid>.ext4`, `ExtraDisks` (EROFS fastpath + ext4 volumes), `Shares` (binds + overflow layers), `Services: nil`.
      - 헬퍼가 VM 부팅 (~0.38 s) → `gvx-agent` `OpHello` (`"containers"` 확인) → `OpDiskMount{Format: true}` (`/dev/vda` 준비).
   5. `vmclient.ContainerStart(ContainerSpec{...})` 호출 → `gvx-agent`가 EROFS 레이어 + `overlayfs` + 바인드/볼륨/tmpfs + `/dev`/`/etc` + cgroup v2 구성 후 컨테이너 PID 1 스폰 (`Privileged` 지정 시 Linux capabilities 전체 유지, 미지정 시 기본 Docker capability 셋 적용).
   6. `vmclient.ContainerAttach` 스트림 연결(stdout/stderr → `Engine.handleRunnerOutput`, stdin 연결) 및 호스트 TCP 포트 포워더 바인딩.
   7. `sandbox.EventChanged(Running)` + `sandbox.EventReady` 발행.
2. **정상 종료 (`docker stop` / PID 1 종료)**
   - `docker stop`: `ContainerSignal(StopSignal)` → `timeout` 초과 시 `SIGKILL`.
   - PID 1 종료 시 `FrameExit{code, signal}` 도착 → 포트 포워더 종료 → `vmclient.Close(graceTimeout)` (`OpShutdown`: 볼륨/디스크 sync + unmount + `poweroff -f`) → 헬퍼 종료 → `sandbox.EventExited` 발행 → Apple VZ XPC 프로세스 즉시 소멸(RAM 100% 회수).
3. **크래시 / 앱 종료**
   - 헬퍼 예기치 않은 종료(`client.Done()`): 포트 포워더·볼륨 락 해제 후 `sandbox.EventExited(ExitCode: 255)` 발행.
   - 앱 종료/SIGKILL: 모든 헬퍼의 stdin lease pipe EOF → 각 헬퍼가 즉시 VM 종료 후 exit (orphan VM 0개).

---

## 6. `gvx-desktop-vm` (`vm-assets-v2` & `v0.2.0`) 설계

### 6.1 `vm-assets-v2` 변경 사항 (`guest/mkassets/`, `vmassets/`)

1. **`guest/mkassets/pins.go` — `requiredModules` 추가**:
   ```go
   var requiredModules = []string{
       "fuse", "virtiofs",
       "binfmt_misc",
       "vsock", "vmw_vsock_virtio_transport",
       "virtio_rng", "virtio_balloon",
       "virtio_net", "af_packet",
       "overlay", "veth",
       "virtio_blk",
       "ext4",
       // v2: micro-VM container runtime (zero-copy EROFS + overlay/VPN networking)
       "erofs",
       "loop",
       "tun",
       "wireguard",
   }
   ```
   - `resolveModules`가 `modules.dep`에서 의존 모듈을 자동으로 위상 정렬해 `load.list`에 기록합니다.
   - `initramfs.gz` 증가량은 약 400–600 KiB 수준으로 부팅 시간(~0.38 s)에 실질적 영향이 없습니다.
2. **`vmassets/vmassets.go`**:
   - `const Version = "2"` (`ReleaseTag = "vm-assets-v2"`, `ArchiveName = "gvx-vm-assets-v2-arm64.tar.gz"`).
   - `vm-assets-v2` 태그 푸시 → `.github/workflows/vm-assets.yml` 2회 재현 빌드 검증 및 릴리즈 발행 → `ArchiveSHA256`, `ArchiveSize` 핀 후 `gvx-desktop-vm v0.2.0` 릴리즈.
   - **빌더 호환성**: `buildkit/` 바이너리와 `ServiceBuildkitd`를 그대로 포함하므로 기존 `amd64 builds with Rosetta`와 캐시 1벌을 공유합니다.

### 6.2 `vmproto` 확장 (`gvx-desktop-vm/vmproto/vmproto.go`)

기존 `Version = 1` 프로토콜과 100% 하위 호환을 유지하며 `Hello.Features = ["containers"]`로 광고합니다.

1. **`VMSpec` 추가 필드**:
   ```go
   type ShareSpec struct {
       Tag      string `json:"tag"`
       Dir      string `json:"dir"`
       ReadOnly bool   `json:"readOnly,omitempty"`
   }

   type ExtraDiskSpec struct {
       Path     string `json:"path"`
       ReadOnly bool   `json:"readOnly,omitempty"`
   }

   type VMSpec struct {
       // ... 기존 필드 ...
       Shares     []ShareSpec     `json:"shares,omitempty"`
       ExtraDisks []ExtraDiskSpec `json:"extraDisks,omitempty"`
   }
   ```
   - `vmhelper/server.go`의 `buildCommandLine`이 `Shares`를 `gvx.shares=tag1:ro,tag2,...`로 직렬화하여 `guest/rootfs/rc`가 `/mnt/<tag>`에 자동 마운트합니다.
   - `Disk` = `/dev/vda`, `ExtraDisks[i]` = `/dev/vdb`, `/dev/vdc`, ... 순서로 매핑됩니다.
   - `Assets`는 컨테이너 Micro-VM에서 생략 가능합니다.
2. **`vmproto` 컨테이너 Ops (헬퍼 ↔ 게스트 에이전트 투명 중계)**:
   - `OpContainerStart` (`container-start`)
   - `OpContainerAttach` (`container-attach`) — 양방향 프레임 스트림
   - `OpContainerSignal` (`container-signal`)
   - `OpContainerResize` (`container-resize`)
   - `OpContainerExec` (`container-exec`) — 양방향 프레임 스트림
   - `OpContainerTop` (`container-top`)
   - `OpContainerStats` (`container-stats`)
   - `OpContainerPause` (`container-pause`) / `OpContainerUnpause` (`container-unpause`)
   - `OpContainerStat` (`container-stat`) / `OpContainerCopyFrom` (`container-copy-from`) / `OpContainerCopyTo` (`container-copy-to`) / `OpContainerExport` (`container-export`)

### 6.3 `guestproto` 및 `guest/agent` 확장 (`gvx-desktop-vm/guestproto`, `guest/agent`)

`guestproto`에 `FeatureContainers = "containers"`, 13개 컨테이너 op, 프레임 확장(`FrameResize = 6`, `FrameData = 7`, `FrameEOF = 8`)을 추가합니다.

```go
type ContainerSpec struct {
    Argv           []string         `json:"argv"`
    Env            []string         `json:"env,omitempty"`
    Workdir        string           `json:"workdir,omitempty"`
    User           string           `json:"user,omitempty"`
    Hostname       string           `json:"hostname,omitempty"`
    ExtraHosts     []string         `json:"extraHosts,omitempty"`
    TTY            bool             `json:"tty,omitempty"`
    Interactive    bool             `json:"interactive,omitempty"`
    Rows           uint16           `json:"rows,omitempty"`
    Cols           uint16           `json:"cols,omitempty"`
    Privileged     bool             `json:"privileged,omitempty"`
    ReadonlyRootfs bool             `json:"readonlyRootfs,omitempty"`
    MemoryMax      int64            `json:"memoryMax,omitempty"`
    CPUQuota       int64            `json:"cpuQuota,omitempty"`
    CPUPeriod      int64            `json:"cpuPeriod,omitempty"`
    PidsLimit      int64            `json:"pidsLimit,omitempty"`
    Layers         []LayerSource    `json:"layers"`
    Mounts         []ContainerMount `json:"mounts,omitempty"`
}

type LayerSource struct {
    Device string `json:"device,omitempty"` // fast path: /dev/vdb .. /dev/vdi
    File   string `json:"file,omitempty"`   // overflow path: /mnt/layers/<hex>.erofs via loop.ko
}

type ContainerMount struct {
    Kind     string `json:"kind"`               // "bind", "volume", "tmpfs"
    Source   string `json:"source,omitempty"`   // virtiofs path, block dev, or ""
    Target   string `json:"target"`             // container path
    ReadOnly bool   `json:"readOnly,omitempty"`
    Options  string `json:"options,omitempty"`  // tmpfs size=/mode=
}
```

### 6.4 게스트 내부 실행 흐름 (`guest/agent/container_linux.go`)

1. **레이어 마운트 & OverlayFS 구성**:
   - 각 `LayerSource`를 `/mnt/l0`, `/mnt/l1`, ... 에 `erofs` (`MS_RDONLY`) 마운트 (`File`인 경우 `LOOP_CTL_GET_FREE` + `LOOP_SET_FD` + `LO_FLAGS_READ_ONLY`).
   - `lowerdir=/mnt/l<N-1>:...:/mnt/l0` (최상단→하단 순).
   - `ReadonlyRootfs == false`: `/var/lib/gvx/upper`, `/var/lib/gvx/work` 생성 후 `overlayfs` → `/mnt/rootfs`.
   - `ReadonlyRootfs == true`: 읽기 전용 `overlayfs` (또는 단일 레이어 `MS_BIND|MS_RDONLY`).
2. **바인드 마운트 / 볼륨 / tmpfs / 특수 파일시스템**:
   - `bind`: 디렉토리 또는 단일 파일 `MS_BIND` (`ReadOnly` 시 `MS_REMOUNT|MS_RDONLY`).
   - `volume`: `/dev/vdX` `ext4` → `/mnt/vol<k>` → `/mnt/rootfs/<target>` `MS_BIND`.
   - `tmpfs`: `mount("tmpfs", "/mnt/rootfs/<target>", "tmpfs", 0, options)`.
   - `/mnt/rootfs/{proc,sys,dev,dev/pts,dev/shm,etc}` 초기화:
     - `/dev` tmpfs + `/dev/{null,zero,full,random,urandom,tty}` + `/dev/pts` + `/dev/shm` + `/dev/fd` → `/proc/self/fd`.
     - `Privileged == true`일 때 `/dev/net/tun` 및 `/dev/fuse` 장치 노드도 노출하여 Tailscale/WireGuard/FUSE 워크로드 지원 (단일 Micro-VM 내부로 격리됨!).
     - `/etc/hostname`, `/etc/hosts` (`127.0.0.1 localhost`, `::1 localhost`, `127.0.0.1 <hostname>`, `<gatewayIP> host.docker.internal`, `ExtraHosts`), `/etc/resolv.conf` (게스트 DHCP `/etc/resolv.conf` 대기 최대 1.5 s 후 복사, 폴백 `1.1.1.1`/`8.8.8.8`).
3. **cgroup v2 및 네임스페이스 격리 (`gvx-agent __ctr-init`)**:
   - `/sys/fs/cgroup/gvx-ctr` 생성 (`memory.max`, `cpu.max`, `pids.max`).
   - `gvx-agent __ctr-init`을 `Cloneflags: CLONE_NEWNS | CLONE_NEWPID | CLONE_NEWUTS | CLONE_NEWIPC`로 스폰:
     - `MS_REC|MS_PRIVATE` on `/` → `pivot_root("/mnt/rootfs", "/mnt/rootfs/.gvx-oldroot")` → `chdir("/")` → `umount2("/.gvx-oldroot", MNT_DETACH)` → `rmdir("/.gvx-oldroot")`.
     - `mount("proc", "/proc", "proc", 0, "")` (새 PID 네임스페이스 `/proc`) 및 `mount("sysfs", "/sys", "sysfs", ...)` (`Privileged` 여부에 따라 rw/ro).
     - `sethostname(Hostname)`, `/etc/passwd` + `/etc/group` 기반 `User` 해석, `Setgroups`/`Setgid`/`Setuid`, `chdir(Workdir)`, `PATH` 탐색 후 `unix.Exec(bin, argv, env)`.
   - TTY 모드: `/dev/ptmx` + `/dev/pts/N` (`Setsid: true, Setctty: true`, 초기 `TIOCSWINSZ`).
   - `OpShutdown`: 컨테이너 정지 → 모든 `volume`·`overlay`·`erofs`·`loop` 언마운트 → `/var/lib/gvx` 언마운트 → `Sync()` → `PowerOff()`.
4. **`docker exec` (`OpContainerExec`)**:
   - Go 멀티스레드 런타임의 `setns(CLONE_NEWNS)` `EINVAL` 제약을 회피하기 위해, 기본 `SysProcAttr.Chroot = "/mnt/rootfs"` + `Credential` + cgroup 배속을 사용하거나, `vm-assets-v2`의 `initramfs`에 `util-linux` `nsenter`를 포함해 `nsenter -t <pid1> -m -p -u -i -- ...`로 컨테이너의 PID/MNT/UTS/IPC 네임스페이스에 100% 진입합니다. (Phase 1에서 `util-linux` 핀 내 `nsenter` 포함 여부를 확인해 확정.)

---

## 7. 스토리지, 볼륨 및 리소스 관리 (`gvx-desktop`)

### 7.1 디렉토리 레이아웃

`vm-assets-v2` EROFS 직접 마운트 덕분에 **이미지 변환 캐시가 필요 없습니다.**

```text
<engineDir>/microvm/
└── rootfs/
    └── <containerID>.ext4        # 컨테이너별 영속 쓰기 ext4 (16 GiB sparse, /dev/vda)

<os.TempDir()>/
└── gvxd-c-<shortContainerID>/    # 0700, 104바이트 sun_path 제한 준수
    ├── helper.sock               # 0600, vmproto 제어 소켓
    └── console.log               # 1 MiB 상한 게스트 콘솔 로그
```

- **영속 쓰기 레이어**: 최초 기동 시 `mke2fs` 포맷, `docker stop`/`start` 유지, `docker rm` 삭제, 엔진 `Open` 시 `sweepMicroVMRootfs`로 고아 파일 정리.
- **Named / Anonymous 볼륨 공유**: `rungv`의 `<VolumeDir>/<name>.ext4`를 `ExtraDisks`로 직접 첨부 + 호스트 `flock`으로 `gvx` 샌드박스와 `microvm` 컨테이너 간 안전한 볼륨 공유 보장.
- **디바이스 슬롯 상한**: EROFS fastpath 최대 8개(초과 시 `virtiofs`+`loop.ko` 최대 128 레이어), 추가 `ext4` 볼륨 최대 8개, `virtiofs` 바인드 최대 12개.

### 7.2 리소스 기본값 및 오버라이드 (`apple/container` 기준)

| 항목 | 컨테이너별 오버라이드 (`HostConfig`) | 전역 기본값 (`store.Settings`) | 시스템 디폴트 |
| :--- | :--- | :--- | :--- |
| **vCPUs** | `NanoCpus` (`ceil(NanoCpus/1e9)`) 또는 `CpuQuota/CpuPeriod` (`[1, NumCPU]`) | `MicroVMCPUs` (`0`이면 디폴트) | **`min(4, runtime.NumCPU())`** |
| **Memory** | `HostConfig.Memory` (`[128 MiB, 75% HostRAM]`) | `MicroVMMemoryMiB` (`0`이면 디폴트) | **`1024 MiB` (`1 GiB`)** |
| **Balloon** | 항상 활성 (`VZVirtioTraditionalMemoryBalloonDevice`) | — | 활성 |
| **PidsLimit** | `HostConfig.PidsLimit` → cgroup v2 `pids.max` | — | 무제한 |
| **RootFS Disk** | — | — | `16 GiB` sparse (`rootfs/<cid>.ext4`) |

---

## 8. 네트워킹 및 포트 포워딩

- **아웃바운드 NAT & `host.docker.internal`**: `VZNATNetworkDeviceAttachment` (`gvx.net=1`), 게스트 DHCP(`192.168.64.x/24`), 게이트웨이(`192.168.64.1`)를 `/etc/hosts`에 `host.docker.internal`로 자동 등록. `--network=none`이면 `Network: false`.
- **인바운드 TCP 포트 퍼블리싱 (`-p`, `-P`)**: 기존 `resolveStartPorts`, `e.ctr.portMu`, `AllowLANPorts` 정책 그대로 재사용. 호스트 TCP 리스너 → `vmclient.Dial(tcp, 127.0.0.1:<guestPort>)` 양방향 파이핑. Labs `*.localhost` HTTPS 프록시 및 내장 MCP 서버가 수정 없이 자동 연동.
- **UDP 포트**: v1에서는 생성 응답 `Warnings` 반환 후 TCP만 포워딩.
- **Compose 사용자 정의 네트워크 (`gvx ↔ microvm` 통신)**:
  - v1(Phase 3)에서는 사용자 정의 네트워크 연결 시 NAT로 실행하며 `Warnings`를 반환.
  - Phase 4에서는 이미 `vm-assets-v2`에 포함된 `tun.ko`/`wireguard.ko`를 이용해 `gvx-agent`가 게이트웨이(`192.168.64.1:<hubPort>`)를 통해 호스트 `overlay.Fabric`에 직접 조인 → `gvx`(`arm64`)와 `microvm`(`arm64`/`amd64`) 컨테이너가 동일한 Compose 네트워크에서 서비스 이름으로 직접 통신(에셋 재배포 불필요).

---

## 9. Sandbox Engine 통합 (`internal/engine`)

### 9.1 `Container` 및 `HostConfig` 확장 ([`internal/engine/container_types.go`](../../internal/engine/container_types.go))

```go
type HostConfig struct {
    // ... 기존 필드 ...
    Runtime string `json:"Runtime,omitempty"`
}

type Container struct {
    // ... 기존 필드 ...
    // Platform is "linux/amd64" or "linux/arm64" (empty means host arch).
    Platform string `json:"platform,omitempty"`
    // Runtime is "gvx" (empty) or "microvm".
    Runtime string `json:"runtime,omitempty"`
}
```

- 기존 `containers.json`과 100% 하위 호환 (빈 `Platform`/`Runtime` = 호스트 아키텍처 + `gvx`).

### 9.2 `Engine` API 및 런타임 위임 (`internal/engine/microvm.go`)

```go
type MicroVMConfig struct {
    Launcher     vmclient.Launcher // <exe> __vm-helper serve
    Assets       func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error)
    RosettaReady func() bool       // amd64 컨테이너 요청 시 Rosetta 설치 여부 확인
    DefaultCPUs  int               // 0 → min(4, NumCPU)
    DefaultMemMB int               // 0 → 1024 (1 GiB)
}

type MicroVMState struct {
    Enabled     bool
    RunningVMs  int
    Downloading bool
    Done, Total int64
    Assets      string // "2", "local"
    Err         string
}
```

- `Engine.SetMicroVM(cfg *MicroVMConfig) error`: ON 시 활성화(`GET /info`의 `Runtimes["microvm"]` 등록), OFF 시 실행 중인 모든 Micro-VM 컨테이너(`arm64`·`amd64`)를 graceful stop.
- `Engine.MicroVMState() MicroVMState`: Labs UI 상태 표시.
- **런타임 위임**: `c.Runtime == "microvm"`인 컨테이너의 모든 수명 주기·I/O·제어 호출(`Start`, `Stop`, `Shutdown`, `Signal`, `State`, `Stdin`, `CloseStdin`, `Resize`, `Forget`, `Control`, `Pause`, `Unpause`)을 `microVMManager`로 라우팅하고, `sandbox.Event`와 `sandbox.Output`을 기존 `Engine.onRunnerEvent` / `Engine.handleRunnerOutput` 파이프라인에 주입.
- **UI Terminal 탭**: `microvmControl.ExecArgv`가 `<exe> __vm-helper exec -socket <helper.sock> ...`를 반환해 [`internal/term`](../../internal/term) PTY 터미널이 코드 수정 없이 연결.
- **`docker inspect`**: `Driver: "gvx-microvm"`, `HostConfig.Runtime: "microvm"`, `Platform: "linux"`.
- **`withUpper` (`diff`/`export`/`commit`)**: 실행 중인 Micro-VM 컨테이너는 `OpContainerExport(upperOnly: true)`로 모두 지원; 중지된 Micro-VM 컨테이너는 409 안내 반환.

---

## 10. 설정 및 Labs UI 설계 (`internal/store`, `internal/ui`)

### 10.1 `store.Settings` 확장 ([`internal/store/store.go`](../../internal/store/store.go))

```go
const (
    DefaultMicroVMCPUs      = 4
    DefaultMicroVMMemoryMiB = 1024
)

type Settings struct {
    // ... 기존 필드 ...
    // MicroVMContainers enables the per-container Linux micro-VM runtime
    // (Labs, Apple silicon macOS): runs linux/amd64 containers automatically
    // with Rosetta, and runs linux/arm64 containers on a real Linux 6.18
    // kernel when --runtime=microvm is requested.
    MicroVMContainers bool `json:"microVmContainers,omitempty"`
    MicroVMCPUs       int  `json:"microVmCpus,omitempty"`      // 0 → min(4, NumCPU)
    MicroVMMemoryMiB  int  `json:"microVmMemoryMiB,omitempty"` // 0 → 1024 (1 GiB)
}
```

### 10.2 Labs 페이지 — 동일 카테고리 내 별도 토글 ([`internal/ui/page_labs.go`](../../internal/ui/page_labs.go), [`internal/ui/page_settings.go`](../../internal/ui/page_settings.go))

Apple Silicon macOS(`amd64BuilderPlatform() == true`)에서 표시되는 기존 `Cross-architecture builds` 그룹을 **`Cross-architecture & micro-VMs`** 그룹으로 확장하고, 공유 요구사항 헤더 아래에 두 개의 독립 스위치를 배치합니다.

```text
┌─ [Cpu] Cross-architecture & micro-VMs ───────────────────────────────────────┐
│  ● Apple silicon · macOS 15.x · Rosetta installed         [Install Rosetta*] │
│  (Sandbox Engine 비활성 시: Requires the default sandbox engine...)           │
│                                                                              │
│  amd64 builds with Rosetta                                        [ ○ / ● ]  │
│  Builds for --platform linux/amd64 run in a Linux VM with Rosetta. The VM    │
│  starts on the first amd64 build and stops when idle.                        │
│  (활성 시 기존 빌더 상태 카드 · buildx 안내 · Reset amd64 build cache)        │
│  ──────────────────────────────────────────────────────────────────────────  │
│  Micro-VM container runtime                                       [ ○ / ● ]  │
│  Run linux/amd64 containers automatically with Rosetta, or run linux/arm64   │
│  containers on a real Linux kernel with --runtime=microvm. Each container    │
│  gets an isolated VM and returns all memory when stopped.                    │
│                                                                              │
│  Default resources per VM        CPUs [ 4 ]   Memory (MiB) [ 1024 ]          │
│  (Overrides via docker run --cpus / -m. Defaults match apple/container.)     │
│                                                                              │
│  (활성 시 상태 카드)                                                          │
│  ● 2 micro-VMs running · VM assets v2                                        │
│  (Rosetta 미설치 시: arm64 --runtime=microvm ready; install Rosetta for amd64)│
│  (최초 다운로드 시: Downloading the VM, 24 of 74 MB)                          │
└──────────────────────────────────────────────────────────────────────────────┘
```

- **게이팅 규칙**:
  - `amd64 builds with Rosetta`: 기존과 동일하게 `SandboxEngine && AppleSilicon && macOS 13+ && Virtualization && RosettaInstalled`.
  - `Micro-VM container runtime`: `SandboxEngine && AppleSilicon && macOS 13+ && Virtualization` (Rosetta 미설치 상태에서도 `arm64` `--runtime=microvm` 용도로 ON 가능! `amd64` 실행 시점에만 Rosetta 확인).
- **Images 페이지 ([`internal/ui/page_storage.go`](../../internal/ui/page_storage.go))**:
  - `amd64` 이미지: `MicroVMContainers` ON 시 배지 툴팁 `"Built for linux/amd64; runs in a Rosetta micro-VM."` 및 `Copy docker run command` (`docker run --rm -it --platform linux/amd64 <ref>`) 노출.
  - `arm64` 이미지: `MicroVMContainers` ON 시 `More actions` 메뉴에 `Copy docker run (--runtime=microvm)` 추가 (호환성 문제 시 1클릭 복사).
- **Containers 페이지 ([`internal/ui/page_containers.go`](../../internal/ui/page_containers.go))**:
  - `microvm` 컨테이너 행에 `micro-VM` 배지 표시 (`amd64` 컨테이너는 `amd64` 배지 병기) → `gvx` 샌드박스 컨테이너와 한눈에 구분.
- **i18n**: 신규 문자열 전부를 `tr()`/`trn()`으로 작성하고 6개 카탈로그(`ko`, `ja`, `zh-Hans`, `zh-Hant`, `de`, `hi`)에 반영, `ggui i18n check` 통과.

---

## 11. 보안 모델 및 격리 경계

1. **하드웨어 가상화 격리**: 컨테이너당 독립 VZ VM + 독립 Linux 커널·메모리. `--privileged`를 부여한 `arm64` Micro-VM 컨테이너도 호스트 macOS나 다른 컨테이너에 영향을 줄 수 없음.
2. **제어 평면 격리**: `gvx-agent` vsock 리스너는 `VMADDR_CID_HOST (2)` 피어만 허용; `pivot_root` + `umount2(oldroot, MNT_DETACH)` 이후 `/mnt/layers`, `/mnt/l*`, 호스트 바인드 부모 디렉토리, `/var/lib/gvx` 접근 불가; 호스트 소켓 디렉토리 `0700` / 소켓 `0600`.
3. **바인드 마운트 & 볼륨 보호**: `dockerproxy` `DockerSharedFolders` 검증 유지, 단일 파일 바인드 부모 디렉토리 분리, `rungv` named volume `.ext4` 호스트 `flock`으로 `gvx` ↔ `microvm` 동시 쓰기 손상 차단.
4. **네트워크 노출 통제**: `AllowLANPorts` OFF 시 모든 퍼블리시 포트를 `127.0.0.1`에만 바인딩.

---

## 12. 패키지 레이아웃 및 구현 단계

### 12.1 `ironpark/gvx-desktop-vm` (`v0.2.0` / `vm-assets-v2`)

```text
guest/mkassets/pins.go             requiredModules에 erofs, loop, tun, wireguard 추가
guest/agent/
├── server.go                      Hello.Features: ["containers"], 컨테이너 ops 라우팅
├── container_linux.go             EROFS(blk/loop) + overlayfs + binds/vols + pivot_root + PTY + exec + cgroup + cp/export
├── container_other.go             비-Linux 스텁
├── user_linux.go                  /etc/passwd, /etc/group 파서
└── container_test.go              단위 테스트 + opt-in VZ 실기 부팅 테스트 (arm64 & amd64)
guestproto/guestproto.go           FeatureContainers, ContainerSpec, 컨테이너 ops & 프레임
vmproto/vmproto.go                 VMSpec.Shares/ExtraDisks, 컨테이너 ops
vmhelper/
├── server.go                      Shares → gvx.shares, ExtraDisks → vz.Disks, 컨테이너 ops 프록시
├── agent.go                       guestproto 컨테이너 클라이언트 메서드
└── main.go, exec.go               __vm-helper exec (UI Terminal PTY 연결용)
vmclient/vmclient.go               ContainerStart/Attach/Signal/Resize/Exec/Top/Stats/Pause/Unpause/Copy/Export
vmassets/vmassets.go               Version = "2", vm-assets-v2 SHA256/Size 핀
```

### 12.2 `ironpark/gvx-desktop`

```text
go.mod, go.sum                     github.com/ironpark/gvx-desktop-vm v0.2.0
internal/imagefs/layers.go         ResolveGVXLayers(cacheDir, imageID) ([]string, error)
internal/engine/
├── container_types.go             HostConfig.Runtime, Container.Platform, Container.Runtime
├── system.go                      GET /info Runtimes["microvm"] 등록
├── foreign_images.go              resolveCreateImage / runtime 결정 매트릭스 (§3.2)
├── microvm.go                     SetMicroVM, MicroVMState, Micro-VM 매니저, TCP 포워더, microvmControl
├── microvm_test.go                페이크 vmclient 단위 테스트 (arm64 --runtime=microvm + amd64 + 회귀)
├── containers.go, engine.go, rootfs.go  Runtime == "microvm" 라우팅 및 고아 rootfs 정리
internal/store/
├── store.go, store_test.go        MicroVMContainers, MicroVMCPUs, MicroVMMemoryMiB
internal/ui/
├── model.go, model_docker_proxy.go  MicroVMState 시그널, 엔진 연동
├── model_amd64_builder.go         SetMicroVMContainers, SetMicroVMResources
├── page_labs.go, page_settings.go Cross-architecture & micro-VMs 그룹 내 독립 토글 + 리소스 설정 + 상태 카드
├── page_storage.go, page_containers.go  micro-VM / amd64 배지 및 복사 액션
├── i18n_{ko,ja,zh_Hans,zh_Hant,de,hi}.go  6개 언어 번역
└── model_microvm_test.go          UI 토글·게이팅·ggui_debug 테스트
```

### 12.3 구현 순서

| 단계 | 저장소 | 내용 및 완료 기준 |
| :--- | :--- | :--- |
| **Phase 1: `vm-assets-v2` & `gvx-desktop-vm v0.2.0`** | `gvx-desktop-vm` | `erofs`/`loop`/`tun`/`wireguard` 모듈 추가, `guestproto`/`vmproto`/`guest/agent`/`vmhelper`/`vmclient` 컨테이너 ops 구현, 로컬 `mkassets` 2회 재현성 검증 + 실기 VZ EROFS 마운트 (`arm64` 네이티브 + `amd64` Rosetta 모두) → `vm-assets-v2` 릴리즈 & `v0.2.0` 태그 |
| **Phase 2: Sandbox Engine 통합** | `gvx-desktop` | `go.mod` `v0.2.0` 갱신, `ResolveGVXLayers`, `HostConfig.Runtime` / `Container.Platform` / `Container.Runtime`, `GET /info` `Runtimes`, `microvm.go` (매니저·TCP 포워더·`microvmControl`), 단위 테스트 + `arm64 --runtime=microvm` 및 `amd64` E2E 검증 |
| **Phase 3: Labs UI + Store + i18n** | `gvx-desktop` | `store.Settings` 확장, Labs 동일 카테고리 독립 토글 + 리소스 기본값(`4` vCPU / `1024 MiB`), Images/Containers 배지, 6개 언어 i18n, `gofmt`/`go vet`(3 OS)/`ggui i18n check`/`-tags ggui_debug`/`-race` 검증 |
| **Phase 4 (후속): Compose `gvx ↔ microvm` 오버레이 네트워크** | `gvx-desktop-vm`, `gvx-desktop` | `vm-assets-v2`의 `tun.ko`/`wireguard.ko`로 게스트 `gvx0` 링크를 호스트 `overlay.Fabric`(게이트웨이 `192.168.64.1`)에 연결, `gvx` ↔ `microvm` 컨테이너 간 Compose 서비스 통신 통합 |

---

## 13. 테스트 및 검증 계획

### 13.1 자동화 단위 테스트 (`go test -short`, VM 미사용, 크로스플랫폼)

- **`gvx-desktop-vm`**:
  - `guestproto` / `vmproto` 신규 컨테이너 ops 및 프레임 라운드트립.
  - `guest/agent`: `/etc/passwd` 파싱, `lowerdir` 조립 순서(상단→하단), 마운트/권한 검증, 페이크 백엔드 프로토콜 테스트.
  - `vmhelper` / `vmclient`: `Shares` → `gvx.shares` 커맨드라인 생성, `ExtraDisks` 매핑, 페이크 VM 기반 컨테이너 수명 주기·프록시 테스트.
  - `mkassets`: 결정적 cpio/tar/gzip 재현성 테스트 (`erofs`/`loop`/`tun`/`wireguard` 포함).
- **`gvx-desktop`**:
  - `internal/imagefs`: `ResolveGVXLayers` 단위 테스트 (`testdata/gvxcache`).
  - `internal/engine`:
    - `MicroVMContainers` OFF 시 `amd64` 400 및 `--runtime=microvm` 400 회귀 검증.
    - ON 시 `GET /info`에 `Runtimes["microvm"]` 노출, `arm64` 기본(`gvx`), `arm64 --runtime=microvm`(`Rosetta: false`), `amd64` 기본(`Rosetta: true`), `amd64 --runtime=gvx` 400, 미캐시 `amd64` 404 유도 검증.
    - 페이크 `vmclient` 기반 `create` → `start` → `logs` → `attach` → `exec` → `top` → `stats` → `pause`/`unpause` → `stop` → `rm` 및 `Engine.Close` 정리 검증.
  - `internal/ui`: `SetMicroVMContainers` 게이팅(Rosetta 미설치 시에도 `arm64`용 ON 허용 확인), 리소스 기본값 클램핑, `ggui_debug`, `TestCatalogsAreComplete`.
  - 정적 검증: `gofmt -l .`, `GOOS=darwin|linux|windows go vet ./...`, `CGO_ENABLED=0` 크로스빌드, `ggui i18n check`.

### 13.2 실환경 통합 검증 (옵트인 `GVX_MICROVM_TEST=1`, 임시 디렉토리 전용)

실제 사용자의 `~/.gvx`, `~/.docker`, 실행 중인 샌드박스를 건드리지 않고 임시 `-data-dir` 및 임시 `DOCKER_CONFIG`로 검증합니다.

1. **`arm64` 네이티브 Micro-VM (`--runtime=microvm`)**:
   - `docker run --rm --runtime=microvm alpine uname -rm` → `6.18.64-0-virt aarch64` (`VMSpec.Rosetta == false`, `rungv` 캐시 재사용·재풀 없음).
   - `--runtime=microvm --privileged`에서 `/dev/net/tun` 및 실제 Linux 커널 기능 동작 확인.
2. **`amd64` 자동 Rosetta Micro-VM**:
   - `docker run --rm --platform linux/amd64 alpine uname -m` → `x86_64`, 종료 코드 0, 기동 시간 < 1 s, 종료 후 VZ XPC 프로세스 0개.
3. **다중 레이어 (>8개) overflow 경로**: 10개 이상 레이어 이미지에서 `virtiofs` + `loop.ko` + `erofs` 정상 마운트 및 `pivot_root` 후 `/mnt/layers` 비노출 확인.
4. **웹 서버 & 포트 포워딩**: `docker run -d -p 18088:80 --platform linux/amd64 nginx:alpine` 및 `--runtime=microvm nginx:alpine` → `curl http://127.0.0.1:18088` 200 OK + Labs `https://<name>.localhost` 연동.
5. **대화형 TTY, 시그널 & 런타임 제어**: `-it` 입출력·리사이즈, `docker exec`, `docker top`, `docker stats --no-stream`, `docker pause`/`unpause`, `docker cp`, `docker diff`.
6. **스토리지 영속성 & 볼륨 공유**: 바인드 마운트(디렉토리·단일 파일), `rungv` named volume(`gvx` ↔ `microvm` 공유), `stop` → `start` 쓰기 레이어 유지.
7. **리소스 오버라이드**: `docker run --rm --runtime=microvm -m 512m --cpus 2 alpine nproc` → `2`.
8. **크래시 안전성**: 헬퍼 프로세스 `SIGKILL` 시 Apple VZ XPC 프로세스 즉시 소멸 및 컨테이너 `exited` 전환 확인.
