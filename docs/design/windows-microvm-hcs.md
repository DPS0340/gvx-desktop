# Windows Micro-VM 컨테이너 런타임 설계 (Hyper-V HCS, Labs)

Status: Draft (Option A — Hyper-V Host Compute Service / `vmcompute.dll` direct integration).

Related:
- [`docs/design/amd64-microvm.md`](amd64-microvm.md) — macOS Virtualization.framework 기반 1 Container = 1 Micro-VM 설계 (구현 완료)
- [`docs/design/sandbox-engine.md`](sandbox-engine.md) — 기본 gVisor(`rungv`) Sandbox Engine 및 오버레이 네트워크 설계

---

## 1. 개요 및 목표

GVX Desktop은 Windows에서도 기본 `rungv` gVisor 샌드박스(Windows Hypervisor Platform 기반)로 대부분의 Linux 컨테이너를 가볍고 무권한으로 실행합니다. 그러나 macOS와 마찬가지로 다음 워크로드에서는 실제 Linux 커널이 필요합니다.

1. **gVisor 호환성 한계 워크로드**: `io_uring`, `iptables`/`nftables`, `FUSE`, 컨테이너 내부 `overlayfs`/중첩 네임스페이스, `--privileged` VPN/네트워크 워크로드(Tailscale, WireGuard 등), `systemd`, 미구현 Linux 시스콜, 시스콜 집약적 데이터베이스/빌드.
2. **크로스 아키텍처 워크로드**: Windows `amd64` 호스트에서의 `linux/arm64` 이미지 실행 또는 Windows `arm64` 호스트에서의 `linux/amd64` 이미지 실행.

본 문서는 **옵션 A: Windows Hyper-V Host Compute Service (HCS / `vmcompute.dll`) 직접 호출**을 통해 macOS와 동일한 **1 Container = 1 Micro-VM** 아키텍처를 Windows에 도입하는 설계를 정의합니다. WSL 2나 Docker Desktop 같은 외부 런타임에 의존하지 않으며, 기존 `gvx-desktop-vm` 및 `gvx-desktop` 코드의 **약 85–90%**(`vmproto`, `guestproto`, `guest/agent`, `internal/engine/microvm.go`, Phase 4 vsock 오버레이 네트워크, Labs UI)를 그대로 공유합니다.

### 목표

1. **Pure Go (`CGO_ENABLED=0`), 외부 의존성 0**: `vmcompute.dll` 및 WinSock2 `AF_HYPERV`를 `syscall.NewLazyDLL` / `golang.org/x/sys/windows`로 직접 호출해 기존 Windows 빌드 파이프라인(`CGO_ENABLED=0`)을 그대로 유지합니다.
2. **1 Container = 1 HCS Utility VM**: 컨테이너당 초경량 HCS `LinuxKernelDirect` 유틸리티 VM 1개를 기동하고, 종료 시 VM과 메모리를 Windows에 즉시 반환합니다.
3. **기존 `gvx-agent` · `vmproto` · `guestproto` 100% 재사용**: 호스트↔게스트 제어 평면과 데이터 평면(stdio, exec, cp, export, TCP 포트 포워딩, Phase 4 Compose 오버레이 네트워크)을 모두 **HvSocket (`AF_HYPERV` ↔ 게스트 `hv_sock.ko` `AF_VSOCK` port 1024)** 위로 동일하게 운반합니다.
4. **앱 본체의 무권한 유지**: `HcsCreateComputeSystem`이 요구하는 관리자/SYSTEM 권한을 선택적 온디맨드 Windows Service(`gvx-vmservice`)로 분리하고, 사용자 SID 기반 SDDL ACL Named Pipe로만 통신합니다.
5. **제로-카피 EROFS 레이어 공유**: `rungv`가 캐시한 `.erofs` 레이어를 변환 없이 게스트 `erofs.ko` + `overlay.ko`로 마운트합니다.
6. **Compose 혼합 런타임 일관성**: 하나의 `compose.yaml`에서 `gvx` 샌드박스와 `microvm` 컨테이너가 동일한 사용자 정의 네트워크에서 서비스 이름으로 통신합니다(Phase 4 vsock TUN/DNS 오버레이 재사용).

### 비목표 (Windows v1)

- Windows Home 에디션 중 HCS `LinuxKernelDirect` 서드파티 VM 생성이 OS 정책으로 차단된 빌드에서의 우회 (WSL 2 백엔드는 향후 별도 검토; v1에서는 `__vm-helper check`가 명확히 감지해 안내)
- Windows 네이티브 컨테이너(WCOW) 실행 (Linux 컨테이너 전용)
- GPU 패스스루(DDA/GPU-PV), `--network=host`, 체크포인트 스냅샷

---

## 2. 검증된 기반 사실

### 2.1 Alpine `v3.24` `x86_64` `linux-virt-6.18.64-r0.apk` 커널 검증

macOS `vm-assets-v2`와 동일한 Alpine `v3.24` 핀(`linux-virt-6.18.64-r0.apk`, `x86_64`)을 직접 검사한 결과, **추가 커널 빌드나 외부 패키지 없이** Hyper-V HCS 실행에 필요한 모든 드라이버와 파일시스템 모듈이 이미 포함되어 있습니다.

| 구분 | 드라이버 / 모듈 | 위치 (`linux-virt-6.18.64-r0.apk`) | 역할 |
| :--- | :--- | :--- | :--- |
| **Built-in** | `hv_vmbus.ko` | `modules.builtin` | Hyper-V VMBus 코어 버스 |
| **Built-in** | `hv_netvsc.ko` | `modules.builtin` | Hyper-V 가상 NIC (`eth0`, HNS NAT) |
| **Built-in** | `scsi_mod.ko`, `dax.ko` | `modules.builtin` | SCSI 코어 및 PMem DAX 코어 |
| **Module** | `hv_sock.ko` | `kernel/net/vmw_vsock/hv_sock.ko.gz` | `AF_VSOCK` ↔ 호스트 `AF_HYPERV` (port 1024) |
| **Module** | `hv_storvsc.ko`, `sd_mod.ko` | `kernel/drivers/scsi/{hv_storvsc,sd_mod}.ko.gz` | Hyper-V SCSI VHDX 블록 디바이스 (`/dev/sdX`) |
| **Module** | `hv_balloon.ko`, `hv_utils.ko` | `kernel/drivers/hv/{hv_balloon,hv_utils}.ko.gz` | 동적 메모리 회수(balloon), 시간 동기화, shutdown IC |
| **Module** | `nfit.ko`, `nd_pmem.ko` | `kernel/drivers/{acpi/nfit,nvdimm}/...` | HCS `VirtualPMem` (`/dev/pmemX`) 읽기 전용 이미지 |
| **Module** | `pci-hyperv.ko`, `pci-hyperv-intf.ko` | `kernel/drivers/pci/controller/...` | Hyper-V vPCI 지원 |
| **Module** | `9p.ko`, `9pnet.ko`, `9pnet_fd.ko` | `kernel/fs/9p/`, `kernel/net/9p/` | HCS `Plan9` (`9p2000.L` over `hv_sock` fd) 호스트 공유 |
| **Module** | `erofs.ko`, `loop.ko`, `overlay.ko`, `ext4.ko` | `kernel/fs/`, `kernel/drivers/block/` | 제로-카피 EROFS + OverlayFS + 영속 ext4 |
| **Module** | `tun.ko`, `wireguard.ko`, `binfmt_misc.ko` | `kernel/drivers/net/`, `kernel/fs/` | Phase 4 Compose 오버레이, VPN 워크로드, 크로스 아키텍처 |

### 2.2 HCS `LinuxKernelDirect` 커널 포맷 제약 (`bzImage` → `vmlinux`)

- Alpine `linux-virt` 패키지의 `boot/vmlinuz-virt`는 압축된 x86_64 `bzImage`입니다.
- Windows HCS Schema v2.2의 `VirtualMachine.Chipset.LinuxKernelDirect.KernelFilePath`는 **비압축 ELF `vmlinux`**(x86_64)를 요구합니다(WSL 2 / `hcsshim` LCOW 커널과 동일).
- **해결**: `guest/mkassets`가 순수 Go로 `bzImage` 내부의 압축 커널 페이로드(매직 시그니처 탐색 후 gzip/zstd/xz 해제)에서 결정적(byte-identical) ELF `vmlinux`를 추출해 에셋 디렉토리에 `vmlinux`로 저장합니다. `x86_64` ELF 헤더(`EM_X86_64`)와 엔트리포인트를 빌드 시점에 단위 테스트로 검증합니다.
- Windows `arm64` 호스트용 에셋은 macOS와 동일한 bare `Image`(또는 HCS ARM64 요구 포맷 검증 후 핀)를 사용합니다.

### 2.3 Windows HCS Schema v2.2 & `vmcompute.dll` API

`vmcompute.dll`은 Windows 10 1809+ / Windows 11에 기본 탑재된 시스템 DLL로, C API가 JSON 문서 기반으로 매우 작습니다.

| `vmcompute.dll` 함수 | 역할 |
| :--- | :--- |
| `HcsCreateOperation` / `HcsWaitForOperationResult` / `HcsCloseOperation` | 비동기 HCS 연산 핸들 생성·대기·해제 |
| `HcsCreateComputeSystem` | JSON Schema v2.2 문서로 Utility VM 생성 |
| `HcsStartComputeSystem` | VM 부팅 시작 |
| `HcsShutdownComputeSystem` / `HcsTerminateComputeSystem` | 정상 종료(ACPI/IC) 또는 즉시 강제 종료 |
| `HcsGetComputeSystemProperties` / `HcsModifyComputeSystem` | 상태 조회 및 런타임 디바이스 핫플러그/해제 |
| `HcsRegisterComputeSystemCallback` | VM 비정상 종료·크래시 이벤트 수신 (`Done()` 채널 연동) |
| `HcsEnumerateComputeSystems` | 서비스 재시작 시 고아 `gvx-c-*` VM 탐지 및 정리 |
| `HcsCloseComputeSystem` | 핸들 해제 |

---

## 3. 아키텍처 개요 및 추상화 경계

### 3.1 플랫폼 간 공통 vs 전용 레이어

```text
┌────────────────────────────────────────────────────────────────────────────┐
│ gvx-desktop (유저 권한 GUI/트레이 앱, macOS & Windows 100% 공통)           │
│   Docker API (/containers, /networks, /info) → internal/engine/microvm.go │
│   Overlay Fabric (internal/overlay) ←── Phase 4 vsock TUN/DNS 릴레이       │
└───────────────────────────────┬────────────────────────────────────────────┘
                                │ vmproto (JSON + guestproto frames)
        ┌───────────────────────┴────────────────────────┐
        │ macOS: Unix socket (0600)                      │ Windows: Named Pipe (SDDL: User SID + SYSTEM)
        ▼                                                ▼
┌───────────────────────────────┐        ┌──────────────────────────────────────────┐
│ <app> __vm-helper serve       │        │ gvx-vmservice (LocalSystem 서비스)       │
│ (유저 권한 자식 프로세스)      │        │   └── 컨테이너별 VM 세션 (in-process 또는 │
│                               │        │       자식 __vm-helper serve -hcs)       │
│ vmhelper.Server               │        │ vmhelper.Server                          │
│   └── vmInstance = vz.VM      │        │   └── vmInstance = hcs.VM                │
└──────────────┬────────────────┘        └─────────────────────┬────────────────────┘
               │ virtio-vsock (port 1024)                      │ HvSocket AF_HYPERV (port 1024)
               ▼                                               ▼
┌───────────────────────────────────────────────────────────────────────────────────┐
│ Linux 6.18.64-0-virt Micro-VM (macOS arm64 / Windows amd64·arm64)                 │
│   /bin/gvx-agent (guestproto v1 + FeatureContainers + FeatureOverlay)             │
│   ├── EROFS layers + overlayfs + pivot_root + cgroup v2 + PTY/exec/top/stats/cp   │
│   └── gvx0 (tun.ko) + 127.0.0.11:53 DNS relay → vsock/HvSocket → host Fabric      │
└───────────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 `gvx-desktop-vm` 백엔드 추상화

현재 [`vmhelper/server.go`](../../../gvx-desktop-vm/vmhelper/server.go)는 이미 6-메서드 `vmInstance` 인터페이스를 사용하고 있습니다.

```go
type vmInstance interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Done() <-chan struct{}
    Err() error
    Connect(ctx context.Context, port uint32) (net.Conn, error)
    Close() error
}
```

Windows 지원을 위해 `vmhelper`의 `vz.Config` 의존성을 백엔드 중립 패키지 `vmbackend` (또는 `vmhelper` 내부 `vmConfig` + `newPlatformVM` 빌드 태그 분리)로 옮깁니다.

- `vmhelper/backend_darwin.go` (`//go:build darwin`): `vz.New(cfg)` 호출
- `vmhelper/backend_windows.go` (`//go:build windows`): `hcs.New(cfg)` 호출
- `vmhelper/backend_other.go` (`//go:build !darwin && !windows`): `ErrUnsupported` 반환

신규 패키지 `github.com/ironpark/gvx-desktop-vm/hcs` (`//go:build windows` + 순수 Go JSON/VHDX 단위 테스트는 전 OS 실행):

```text
hcs/
├── dll_windows.go        # vmcompute.dll lazy DLL + Hcs* 래퍼 + HcsResult 에러 해석
├── schema.go             # HCS Schema v2.2 JSON 타입 및 빌더 (전 OS 단위 테스트 가능)
├── hvsock_windows.go     # AF_HYPERV (34) 소켓 다이얼 (HV_GUID_VSOCK_TEMPLATE + VM GUID)
├── vm_windows.go         # hcs.VM (vmInstance 구현: Start/Stop/Done/Err/Connect/Close)
├── check_windows.go      # Hyper-V / HCS capability 프로브 (에디션·권한·기능 상태)
├── hns_windows.go        # Default Switch / HNS NAT endpoint 연결 (Outbound 인터넷)
├── vhdx.go               # Pure Go 동적/고정 VHDX 생성기 & raw ext4/EROFS 래퍼 (전 OS 테스트)
└── vhdx_test.go, schema_test.go
```

---

## 4. 권한 경계 및 Windows Service 설계

macOS `Virtualization.framework`와 Windows HCS의 가장 큰 차이는 **권한 요구사항**입니다.

- `HcsCreateComputeSystem`은 `LocalSystem` 또는 상승된 `Administrators` 토큰이 필요합니다.
- GVX Desktop 본체(`GVX Desktop.exe`)는 트레이/GUI 앱으로서 **항상 일반 유저 권한**으로 실행되어야 하며, UAC 상승 실행을 강요하면 안 됩니다.

### 4.1 선택적 온디맨드 Windows Service (`GVXMicroVM`)

Labs에서 사용자가 **Micro-VM container runtime**을 처음 활성화할 때(또는 체크 카드의 **Install VM service** 버튼 클릭 시) UAC 1회 승인으로 경량 Windows Service를 등록합니다.

- **서비스 이름**: `GVXMicroVM` (표시명: `GVX Desktop Micro-VM Service`)
- **실행 바이너리**: `<installDir>\GVX Desktop.exe __vm-helper service`
  - 단일 바이너리 재사용(별도 `.exe` 추가 없음).
  - 시작 유형: `SERVICE_DEMAND_START` (평소에는 꺼져 있다가 사용자가 Micro-VM 기능을 켤 때만 기동, 리소스 소비 0).
- **설치/제거 서브커맨드**:
  - `__vm-helper install-service`: UAC (`ShellExecuteW("runas")`)로 실행되어 SCM에 서비스 등록 및 시작.
  - `__vm-helper uninstall-service`: 실행 중인 `gvx-c-*` HCS VM을 모두 종료하고 서비스 제거.
  - Chocolatey 제거 스크립트([`release/choco.sh`](../../release/choco.sh)의 `chocolateyuninstall.ps1`)에도 서비스 중지·제거 단계 추가.

### 4.2 IPC 및 보안 경계 (SID-ACL Named Pipe + 경로 검증)

유저 프로세스(`gvx-desktop`)와 `LocalSystem` 서비스(`GVXMicroVM`) 간 권한 상승(EoP) 취약점을 원천 차단해야 합니다.

1. **사용자별 SID SDDL ACL Named Pipe**:
   - 서비스는 부트스트랩 파이프 `\\.\pipe\gvx-vmservice`를 열되, 연결 수락 직후 `GetNamedPipeClientProcessId` + `OpenProcessToken` + `GetTokenInformation(TokenUser)`로 호출자의 사용자 SID와 세션 ID를 확인합니다.
   - 또는 설치 시 기록된 승인 사용자 SID(및 `BUILTIN\Administrators`)에만 `GENERIC_READ|GENERIC_WRITE`를 허용하는 SDDL(`D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;<UserSID>)`)을 파이프 생성 시점에 커널 레벨로 강제합니다.
   - 컨테이너당 세션은 멀티플렉싱(서비스 내부에서 컨테이너 ID별 `vmhelper.Server` 인스턴스 생성 → 컨테이너 전용 SDDL Named Pipe `\\.\pipe\gvx-vmhelper-<sid>-<shortID>` 생성)으로 기존 `vmclient.Client`와 1:1 매핑됩니다.
2. **호스트 파일 경로 보안 (Impersonation + Symlink/Traversal 차단)**:
   - `LocalSystem` 서비스가 유저가 전달한 임의 경로(예: `C:\Windows\System32` 또는 junction/symlink가 가리키는 시스템 파일)를 VM에 마운트하거나 생성하면 **로컬 권한 상승(LPE)**이 발생합니다.
   - **필수 방어 규칙**:
     - `Kernel`, `Initrd`: 서비스가 직접 `vmassets` 체크섬(SHA-256)을 검증한 캐시 파일만 허용.
     - `Disk` (컨테이너 writable `.vhdx`), `ExtraDisks` (EROFS 레이어·볼륨), `Shares` (호스트 바인드 마운트): **파일 열기·생성·stat 검증을 반드시 `ImpersonateNamedPipeClient` 토큰 컨텍스트 안에서 수행**하고, `GetFinalPathNameByHandleW`로 해석된 실제 경로에 대해 호출자 유저의 읽기/쓰기 권한이 확인된 파일만 HCS에 전달합니다.
     - `RevertToSelf()` 이후에 열린 핸들/검증된 경로만 HCS API에 넘겨, 일반 유저가 접근 불가능한 시스템 파일은 절대 VM에 노출되지 않습니다.
3. **고아 VM 자동 회수**:
   - 클라이언트의 제어 파이프(또는 heartbeat 파이프)가 닫히면 서비스가 해당 컨테이너의 HCS VM을 즉시 정상 종료/강제 종료합니다(macOS의 stdin EOF 종료와 동일한 크래시 안전성).
   - 서비스 시작 시 `HcsEnumerateComputeSystems`로 `gvx-c-*` 접두사 고아 VM을 모두 종료합니다.
4. **개발/CI 직접 실행 Fallback**:
   - 현재 프로세스가 이미 상승된 관리자 권한(예: CI runner 또는 관리자 터미널 `go test`)이면 서비스 없이 `__vm-helper serve`가 직접 HCS VM을 생성할 수 있도록 허용합니다.

---

## 5. Windows SKU / 기능 요구사항 감지 (`__vm-helper check`)

### 5.1 `vmhelper.CheckResult` 플랫폼 중립 확장

기존 [`vmhelper/api.go`](../../../gvx-desktop-vm/vmhelper/api.go)의 JSON 필드를 하위 호환 유지하면서 Windows 필드를 추가합니다.

```go
type CheckResult struct {
    // 기존 macOS 필드 (유지)
    AppleSilicon   bool   `json:"appleSilicon"`
    MacOS          string `json:"macOS"`
    Virtualization bool   `json:"virtualization"`
    Rosetta        string `json:"rosetta"`

    // 플랫폼 중립 / Windows 확장 필드 (omitempty)
    Backend        string `json:"backend,omitempty"`        // "vz" | "hcs" | ""
    WindowsBuild   string `json:"windowsBuild,omitempty"`   // 예: "10.0.26100"
    WindowsEdition string `json:"windowsEdition,omitempty"` // 예: "Professional", "Core"
    HyperVEnabled  bool   `json:"hyperVEnabled,omitempty"`  // Hyper-V / vmcompute 서비스 가용
    ServiceReady   bool   `json:"serviceReady,omitempty"`   // GVXMicroVM 서비스 응답 가능 (또는 관리자 권한)
    ServiceState   string `json:"serviceState,omitempty"`   // "running" | "stopped" | "not-installed" | "elevated"

    Err string `json:"err,omitempty"`
}
```

### 5.2 프로브 절차 (`hcs/check_windows.go`)

1. `RtlGetVersion`으로 Windows 빌드 번호 확인 (최소 Windows 10 1903 / Build 18362+, 권장 Windows 11 22H2+).
2. `vmcompute.dll` 로드 및 `vmcompute` 서비스 상태(`OpenSCManager` / `QueryServiceStatusEx`) 확인.
3. Hyper-V 하이퍼바이저 활성 여부 확인 (`IsProcessorFeaturePresent` + `HypervisorPresent` 레지스트리/CPUID 또는 경량 HCS 열거 프로브).
4. `GVXMicroVM` 서비스 설치·실행 상태 확인 (`not-installed` / `stopped` / `running`, 또는 현재 프로세스가 이미 관리자면 `elevated`).
5. Windows Home(`EditionID == "Core"`)에서 `vmcompute` 부재 또는 `HcsCreateComputeSystem` `E_ACCESSDENIED`/`HCS_E_HYPERV_NOT_INSTALLED` 감지 시 명확한 안내 메시지 제공:
   - `"Hyper-V Host Compute Service requires Windows Pro, Enterprise, or Education with Hyper-V enabled."`

---

## 6. 스토리지 및 EROFS 제로-카피 전략

macOS(`virtio-blk` + `virtiofs`)와 Windows HCS(`VirtualPMem` / `Scsi` + `Plan9`)의 스토리지 매핑은 다음과 같습니다.

| 용도 | macOS (`vz`) | Windows (`hcs`) | 게스트 경로/마운트 |
| :--- | :--- | :--- | :--- |
| **컨테이너 영속 쓰기 레이어** | raw sparse `.ext4` (`/dev/vda`) | 동적 `.vhdx` (내부 ext4, SCSI `0:0`) | `/dev/sda` → `/var/lib/gvx` |
| **Named/Anon `ext4lite` 볼륨** | `rungv` `<vol>.ext4` (`/dev/vdX`) | VHDX 래퍼 또는 raw-to-VHDX (SCSI `0:k`) | `/dev/sdX` → `/mnt/vol<k>` |
| **EROFS 레이어 (≤8개, Fastpath)** | `virtio-blk` ro (`/dev/vdb..i`) | **VirtualPMem** (`nfit`+`nd_pmem` → `/dev/pmemX`) 또는 **Read-only Fixed VHDX** (SCSI) | `/dev/pmem<k>` 또는 `/dev/sd<X>` → `mount -t erofs -o ro` |
| **EROFS 레이어 (>8개, Overflow)** | `virtiofs` (`layers`) + `loop.ko` | **Plan9** (`layers`, `9p2000.L`) + `loop.ko` | `/mnt/layers/<hex>.erofs` → `loop` → `erofs` |
| **호스트 바인드 마운트** | `virtiofs` (`b0..b11`) | **Plan9** (`b0..b11`, `9p2000.L` over `hv_sock`) | `/mnt/b<k>` → `MS_BIND` |

### 6.1 EROFS 레이어 제로-카피 세부 설계

HCS에서 `rungv`의 캐시 파일(`<LayerCacheDir>/<hex>.<fmt>.erofs`)을 복사 없이 마운트하는 경로는 두 단계로 설계합니다.

1. **공통·가장 단순한 기본 경로 (`Plan9` + `loop.ko` — 1..128개 레이어 통합 지원)**:
   - HCS `VirtualMachine.Devices.Plan9.Shares`에 `LayerCacheDir`을 읽기 전용(`Flags: 0x01` ReadOnly)으로 공유하고, 게스트가 `mount -t 9p -o trans=fd,rfdno=...,wfdno=...,version=9p2000.L,ro,msize=1048576`으로 `/mnt/layers`에 마운트합니다.
   - 각 `.erofs` 파일에 `loop.ko` (`LO_FLAGS_READ_ONLY | LO_FLAGS_AUTOCLEAR`)를 연결해 `erofs`로 마운트합니다.
   - **장점**: `.erofs` 파일에 VHDX 헤더/푸터를 덧붙이거나 2 MiB 패딩을 추가할 필요가 **전혀 없으며**, `rungv` 캐시를 100% 제로-카피·무변형으로 그대로 공유합니다.
2. **고성능 Fastpath 최적화 (`VirtualPMem` / Read-only Fixed VHDX — 벤치마크 후 선택 적용)**:
   - HCS `VirtualPMem`은 호스트 파일을 게스트 물리 주소 공간에 DAX 페이지 매핑해 페이지 캐시 이중 복사를 제거합니다(WSL 2 / LCOW의 레이어 마운트 방식과 동일).
   - 단, HCS `VirtualPMem`은 2 MiB 경계 정렬(또는 Fixed VHDX 포맷)을 요구하므로, Windows 실기 벤치마크에서 `Plan9`+`loop.ko` 대비 부팅/I/O 차이가 유의미할 때(예: >100 ms) 캐시 디렉토리에 2 MiB 정렬된 read-only `.vhdx` 사이드카(또는 하드링크/sparse 패딩)를 선택적으로 활성화합니다.

### 6.2 영속 쓰기 레이어 & `ext4lite` 볼륨: Pure Go VHDX (`hcs/vhdx.go`)

- **컨테이너 영속 쓰기 레이어 (`<engineDir>/microvm/rootfs/<cid>.vhdx`)**:
  - HCS SCSI 컨트롤러(`VirtualMachine.Devices.Scsi["0"].Attachments`)는 `.vhdx` 포맷을 요구합니다.
  - `hcs/vhdx.go`에서 Pure Go(~300줄)로 **16 GiB 동적 VHDX**(`VHDX` 스펙 v1.0: 1 MiB 헤더/로그/Region Table/BAT/Metadata, 초기 파일 크기 ~4 MiB sparse)를 생성합니다. 외부 `diskpart`나 PowerShell/Hyper-V 모듈 의존성이 0입니다.
  - 게스트 부팅 시 `/dev/sda`가 보이면 기존 `OpDiskMount` 로직(`blkid` → 미포맷 시 `mke2fs -t ext4` → `mount /var/lib/gvx`)이 **수정 없이 그대로** 동작합니다.
  - (`DataDevice`는 커널 cmdline `gvx.datadev=/dev/sda` 또는 `/dev/vda` 자동 탐색으로 macOS/Windows 공용화!)
- **`rungv` `ext4lite` Named Volume 공유**:
  - Windows에서 `rungv`의 named volume은 raw `.ext4` 파일입니다.
  - HCS SCSI는 raw 파일 직접 첨부를 허용하지 않으므로, 두 가지 옵션 중 선택합니다:
    - **v1 기본**: 볼륨이 `hostfs`(디렉토리)이면 Plan9 공유로 직접 마운트; `ext4lite` raw 파일이면 `<engineDir>/microvm/vol-cache/<name>.vhdx`가 아닌 **고정 VHDX 푸터 없이 Plan9 공유 디렉토리 + `loop.ko`(RW, `LO_FLAGS_AUTOCLEAR`)**로 게스트가 `rungv`의 raw `.ext4` 파일을 **복사·변환 없이 제로-카피 직접 마운트**!
    - **동시 접근 보호**: 호스트에서 `LockFileEx` (`LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY` 또는 공유 잠금)으로 `gvx` 샌드박스와 `microvm` 컨테이너의 동시 쓰기 손상을 차단합니다(`microvm_lock_windows.go`).
    - *(장점: `rungv`의 `<vol>.ext4`와 100% 동일 파일 공유, 데이터 동기화/변환 비용 0, `gvx` ↔ `microvm` 볼륨 즉시 상호운용!)*

### 6.3 호스트 바인드 마운트 (`Plan9`)

- HCS Schema v2.2의 `VirtualMachine.Devices.Plan9`:
  ```json
  "Plan9": {
    "Shares": [
      { "Name": "b0", "Path": "C:\\Users\\dev\\project", "Port": 50001, "Flags": 0 }
    ]
  }
  ```
- 게스트(`guest/agent` 또는 `guest/rootfs/rc`)는 `AF_VSOCK`으로 `CID=2(Host), Port=50001`에 연결한 소켓 fd를 `mount -t 9p -o trans=fd,rfdno=<fd>,wfdno=<fd>,version=9p2000.L,msize=1048576,cache=loose`로 `/mnt/b0`에 마운트합니다.
- 단일 파일 바인드 마운트는 macOS와 동일하게 부모 디렉토리를 별도 태그로 공유한 뒤 파일만 `MS_BIND`하고 `pivot_root(".", ".") + MNT_DETACH`로 부모 디렉토리를 컨테이너에서 완전히 격리합니다.

---

## 7. 게스트 통신 (`HvSocket`), 콘솔, 종료

### 7.1 `HvSocket` (`hcs/hvsock_windows.go`)

Hyper-V는 게스트의 표준 Linux `AF_VSOCK` 포트 `P`를 호스트 WinSock2 `AF_HYPERV` (`34`)의 well-known GUID 템플릿에 자동 매핑합니다.

```text
HV_GUID_VSOCK_TEMPLATE = {00000000-facb-11e6-bd58-64006a7986d3}
ServiceID(port)        = { <port as 32-bit hex> - facb - 11e6 - bd58 - 64006a7986d3 }
  예: port 1024 (0x400) → {00000400-facb-11e6-bd58-64006a7986d3}
```

- 호스트(`hcs.VM.Connect(ctx, 1024)`)는 `WSASocketW(AF_HYPERV, SOCK_STREAM, HV_PROTOCOL_RAW)` + `connect(SOCKADDR_HV{VmId: vmGUID, ServiceId: vsockGUID(1024)})`로 연결합니다.
- 게스트(`gvx-agent`)는 기존 그대로 `AF_VSOCK` `VMADDR_CID_ANY`, `Port=1024`에서 `accept`하고 피어 `CID == 2 (VMADDR_CID_HOST)`만 허용합니다. **게스트 리스너 코드 변경 0줄!**
- HCS `VirtualMachine.Devices.HvSocket.HvSocketConfig.DefaultConnectTimeoutInSeconds` 및 `ServiceTable`에 `ServiceID(1024)`를 `AllowWildcardBinds: true`로 등록합니다.

### 7.2 콘솔 및 정상 종료

- **콘솔 로그**: `VirtualMachine.Devices.ComPorts["0"] = { NamedPipe: "\\\\.\\pipe\\gvx-console-<id>" }`, 커널 cmdline `console=ttyS0` → 호스트가 파이프를 읽어 기존과 동일하게 `console.log` (1 MiB 상한) 기록.
- **정상 종료**: `OpShutdown` → 게스트 `gvx-agent`가 컨테이너 정지·언마운트·`Sync()`·`PowerOff()` 수행 → HCS VM 종료 이벤트 수신. 타임아웃 시 `HcsTerminateComputeSystem`으로 즉시 정리.

---

## 8. 네트워킹 및 Compose 혼합 오버레이 통합

1. **아웃바운드 NAT (`eth0`)**:
   - `NetworkMode != "none"` (그리고 internal-only 네트워크가 아닌 경우): HNS Default Switch에 경량 NIC 1개(`VirtualMachine.Devices.NetworkAdapters`) 연결 → 게스트 built-in `hv_netvsc`가 `eth0` 생성 → `udhcpc`로 IP/게이트웨이/DNS 획득.
   - `host.docker.internal`은 `eth0` 기본 게이트웨이(또는 호스트 TCP 프록시)로 자동 등록.
2. **인바운드 TCP 포트 포워딩 (`-p`, `-P`)**:
   - macOS와 100% 동일: 호스트 `127.0.0.1:<hostPort>` 리스너 → `vmclient.Dial(tcp, 127.0.0.1:<guestPort>)` → `HvSocket` → 게스트 `gvx-agent` → 컨테이너 `127.0.0.1:<guestPort>`.
   - Labs `*.localhost` HTTPS 프록시와 내장 MCP 서버가 수정 없이 동작.
3. **Compose 사용자 정의 네트워크 (`gvx ↔ microvm`, Phase 4 공통)**:
   - Phase 4의 **vsock TUN(`gvx0`) + vsock DNS(`127.0.0.11:53`) 릴레이**가 `HvSocket` 위에서 100% 동일하게 동작합니다.
   - 호스트 `overlay.Fabric`은 여전히 `127.0.0.1`에만 바인딩되므로 Windows Defender Firewall 팝업을 유발하지 않으며, 하나의 `compose.yaml`에서 `rungv` 샌드박스와 Hyper-V HCS Micro-VM이 서비스 이름으로 투명하게 통신합니다.

---

## 9. 크로스 아키텍처 지원 (선택)

| 호스트 | 네이티브 Micro-VM | 크로스 아키텍처 Micro-VM |
| :--- | :--- | :--- |
| **Windows `amd64`** | `linux/amd64` (`--runtime=microvm`, 네이티브 속도) | `linux/arm64` (선택적 `qemu-aarch64-static` + `binfmt_misc` `CF` 플래그) |
| **Windows `arm64`** | `linux/arm64` (`--runtime=microvm`, 네이티브 속도) | `linux/amd64` (선택적 `qemu-x86_64-static` + `binfmt_misc` `CF` 플래그) |

- Phase W1–W2에서는 **네이티브 아키텍처 `--runtime=microvm`** (Windows `amd64`에서 실제 Linux `6.18` `x86_64` 커널 실행)을 최우선 구현합니다.
- Phase W3에서 Alpine `qemu-aarch64` 정적 바이너리를 핀하여 `linux/arm64` 이미지 실행 및 BuildKit 크로스 빌드까지 동일 구조로 확장합니다.

---

## 10. 멀티아키텍처 `vm-assets` 확장 계획

현재 [`vmassets/vmassets.go`](../../../gvx-desktop-vm/vmassets/vmassets.go)는 `arm64` 단일 아카이브(`gvx-vm-assets-v2-arm64.tar.gz`)를 전제로 합니다. Windows 도입 시 `vm-assets-v3`에서 아키텍처별 아카이브로 확장합니다.

- **아카이브 명명**:
  - `gvx-vm-assets-v3-arm64.tar.gz` (`Image` + `initramfs.gz` + `buildkit/` — macOS Apple Silicon / Windows ARM64)
  - `gvx-vm-assets-v3-amd64.tar.gz` (`vmlinux` + `initramfs.gz` + `buildkit/` — Windows x64)
  - `gvx-vm-assets-v3-sources.tar.gz` (두 아키텍처의 공통/개별 Alpine APKBUILD 및 GPL 대응 소스)
- **`guest/mkassets`**: `-arch arm64|amd64` 플래그 추가; `pins_arm64.go` / `pins_amd64.go`로 핀 분리; `amd64` 빌드 시 `bzImage` → `vmlinux` 추출 및 Hyper-V 모듈(`hv_sock`, `hv_storvsc`, `sd_mod`, `hv_balloon`, `hv_utils`, `nfit`, `nd_pmem`, `9p`, `9pnet`, `9pnet_fd`, `erofs`, `loop`, `overlay`, `ext4`, `tun`, `wireguard`, `binfmt_misc`) 포함.
- **`guest/rootfs/rc` 플랫폼 중립화**:
  - `gvx.shares=`에서 `virtiofs` 또는 `9p` 공유 타입 구분(`tag[:ro]` vs `9p:tag:port[:ro]`).
  - `gvx.datadev=`(기본 `/dev/vda`, Windows HCS는 `/dev/sda`) 지원.
  - 콘솔: macOS `console=hvc0`, Windows HCS `console=ttyS0`.

---

## 11. `gvx-desktop` 통합 (Engine, Store, Labs UI, Installer)

1. **`internal/engine`**:
   - `microvm.go`의 스토리지 플래닝에서 `runtime.GOOS == "windows"`일 때:
     - 영속 쓰기 디스크 경로 `<engineDir>/microvm/rootfs/<cid>.vhdx`
     - EROFS 레이어는 `layers` Plan9 공유 + `File: "/mnt/layers/<rel>"` 경로 사용
     - `ext4lite` 볼륨은 볼륨 디렉토리 Plan9 공유 + `File` 기반 `loop.ko` 마운트 (또는 VHDX) + `LockFileEx` 잠금(`microvm_lock_windows.go`)
   - `foreign_images.go`의 라우팅 매트릭스(§3.2)를 호스트 아키텍처(`hostImageArch`) 기준으로 일반화.
2. **`internal/store`**: 기존 `MicroVMContainers`, `MicroVMCPUs`, `MicroVMMemoryMiB` 그대로 재사용 (추가 스키마 변경 없음).
3. **`internal/ui`**:
   - `page_labs.go`: `amd64BuilderPlatform()` (Apple Silicon 전용)과 분리해 `microVMPlatform()` (`darwin/arm64 || windows/amd64 || windows/arm64`) 도입.
   - Windows에서는 Labs에 **Micro-VM container runtime (Hyper-V)** 카드 표시:
     - 요구사항 상태: `Windows 11 Pro · Hyper-V enabled · VM service running`
     - 서비스 미설치 시 `[Install VM service]` 버튼(UAC 1회 승인) 노출
     - 리소스 기본값(`4` vCPUs, `1024 MiB` RAM) 및 실행 중 VM 수 표시 동일.
4. **패키징 ([`release/choco.sh`](../../release/choco.sh))**:
   - `chocolateyuninstall.ps1`에 `GVXMicroVM` 서비스가 설치되어 있으면 중지·제거하는 정리 단계 추가.

---

## 12. 구현 단계 및 테스트 전략

| 단계 | 저장소 | 범위 및 완료 기준 |
| :--- | :--- | :--- |
| **Phase W1: 멀티아키텍처 `vm-assets` & `hcs` 코어** | `gvx-desktop-vm` | `mkassets -arch amd64` (`vmlinux` 추출 + Hyper-V/9p/EROFS 모듈, 2회 재현성 검증), `hcs/` 패키지(`vmcompute.dll`, Schema v2.2, `HvSocket`, `vhdx.go`, `check_windows.go`), `vmhelper` 백엔드 추상화(`vz` / `hcs`) |
| **Phase W2: `GVXMicroVM` 서비스 & 보안 경계** | `gvx-desktop-vm` | `__vm-helper service` / `install-service` / `uninstall-service`, SDDL Named Pipe, `ImpersonateNamedPipeClient` 경로 검증, 고아 VM 회수, `vmclient` Named Pipe 전송 지원 |
| **Phase W3: `gvx-desktop` Windows Engine & Labs UI** | `gvx-desktop` | `microvm_lock_windows.go`, Plan9 EROFS/바인드/볼륨 플래닝, Labs Windows UI + 6개 언어 i18n, `release/choco.sh` 언인스톨 정리 |
| **Phase W4 (선택): Windows 크로스 아키텍처 (`arm64` on `amd64`)** | 양쪽 레포 | `qemu-aarch64-static` + `binfmt_misc` 등록, Windows `amd64`에서 `--platform linux/arm64` 자동 Micro-VM 실행 |

### 검증 계획

1. **크로스플랫폼 단위 테스트 (`go test -short`, macOS/Linux/Windows CI)**:
   - `hcs/schema_test.go`: HCS Schema v2.2 JSON 직렬화 (`LinuxKernelDirect`, `Scsi`, `Plan9`, `HvSocket`, `ComPorts`, `NetworkAdapters`) 검증.
   - `hcs/vhdx_test.go`: Pure Go 동적 VHDX 생성물의 VHDX v1.0 헤더 체크섬(CRC32C), Region Table, BAT, Metadata 무결성 검증.
   - `mkassets`: `amd64` `bzImage` → `vmlinux` ELF 검증 및 2회 빈 캐시 재현성 검증.
   - `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./... && go build .` 교차 컴파일 검증.
2. **Windows 실기 통합 테스트 (`GVX_MICROVM_TEST=1`, Windows 11 Pro + Hyper-V)**:
   - 네이티브 `linux/amd64` `--runtime=microvm` 부팅(`6.18.64-0-virt x86_64`), 부팅 시간 < 1.5 s, 종료 후 `HcsEnumerateComputeSystems` 고아 VM 0개.
   - `Plan9` + `loop.ko` EROFS 마운트(단일 & 10+ 레이어), 바인드 마운트(디렉토리·단일 파일), `ext4lite` 볼륨 영속성.
   - `GVXMicroVM` 서비스 권한 경계 테스트(비권한 유저가 접근 불가 경로 마운트 요청 시 `Access Denied` 거부 확인).
   - Compose 혼합 스택(`rungv` + `microvm`) 간 Phase 4 vsock 오버레이 통신 및 서비스 이름 DNS 해석 검증.
