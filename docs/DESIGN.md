# gvx Desktop — 조사 · 기능 분류 · UX 설계

## 1. gvx 조사 요약

### 1.1 무엇인가
`gvx`는 Google gVisor의 사용자 공간 커널(Sentry + netstack)을 Windows·macOS·Linux로 포팅해,
**리눅스 VM·커널 드라이버·백그라운드 데몬 없이** OCI 이미지와 Linux ELF 바이너리를
**권한 없는 단일 호스트 프로세스** 안에서 실행하는 런타임이다. CLI는 `rungv`, Go SDK는 `pkg/gvx`.

| 항목 | gvx | Docker Desktop / WSL2 / Lima |
| --- | --- | --- |
| 실행 모델 | 샌드박스 1개 = 호스트 프로세스 1개 | 상주 Linux VM + 데몬 |
| 관리자 권한 | 불필요 | 초기 설정 시 필요 |
| 콜드 스타트 | ~20ms (캐시된 EROFS) | 0.8–2.5s |
| 기본 메모리 | ~16–30MiB / 샌드박스 | 512MiB+ (VM 예약) |
| 엔진 | macOS `hvf`, Windows `whpx`(+`systrap`), Linux `systrap`/`kvm` | 하이퍼바이저 VM |

### 1.2 핵심 기능
- **OCI 실행**: `rungv run [flags] IMAGE [CMD]` — Docker 호환 플래그(`-e -p -v --mount --tmpfs -m --cpus -u -w -h --network --platform --pull`).
- **포트 공개**: `-p`, 범위, `unix:PATH:PORT`(호스트 유닉스 소켓). **샌드박스 시작 전에 모두 바인딩**되어야 함(이후 lockdown).
- **스토리지**: 이미지 레이어 → `.erofs` 캐시, 명명 볼륨 → 단일 파일 `ext4lite`(`<name>.ext4`, 저널링, 자동 확장), 호스트 디렉터리(`hostfs`, `-workspace`), `-rootfs-volume`(루트 FS 영속화). 볼륨은 **동시에 1개 샌드박스만** 사용 가능(파일 락).
- **네트워크**: `nat`(기본, 사용자 공간 TCP/IP), `wg`(WireGuard 터널), `none`, `-isolate-lan`.
- **보안**: 게스트 실행 전 호스트 self-lockdown(macOS Seatbelt / Linux Landlock+seccomp / Windows Restricted Token+Job). `--privileged`는 샌드박스 내부에만 영향.
- **Checkpoint/Restore**: `-control-socket` + `-checkpoint-dir`를 실행 시 미리 열어두고 `rungv checkpoint` → `rungv restore`.
- **이미지/볼륨/시스템 관리**: `image ls|pull|rm|inspect|tag|save|load|prune`, `volume create|ls|inspect|rm|prune`, `system info|df|prune`, `login/logout`. 대부분 `--format json` 지원.
- **대표 워크로드**: `docker:dind`로 **완전한 Docker Engine**(호스트 `docker` CLI가 유닉스 소켓 컨텍스트로 접속), `rancher/k3s`로 **Kubernetes**(단일/다중 노드, flannel/Calico).

### 1.3 데스크톱 앱 설계에 결정적인 제약
1. **데몬이 없다** — 모든 `run`은 포그라운드, `ps/exec/logs/stop/-d` 없음. → **앱이 곧 supervisor**가 되어 샌드박스 프로세스를 소유·관리해야 한다.
2. **SDK(`pkg/gvx`)는 호출 프로세스 자체를 lockdown** 한다. GUI 프로세스에 임베드하면 GUI가 파일·소켓·프로세스를 열 수 없게 된다. → **`rungv` 자식 프로세스를 띄우고 stdout/stderr·종료코드·JSON 출력을 파싱**하는 구조.
3. **포트·마운트는 시작 시 고정** — 생성 폼에서 "나중에 추가 불가"를 명확히 안내하고, 레시피(Docker/K8s)는 포트 범위를 미리 열어둔다.
4. **macOS는 `com.apple.security.hypervisor` 서명 필요** — `rungv info`의 `AvailableBackends`가 비면 서명 안내/원클릭 서명 제공.
5. 볼륨 단일 사용 락, 종료코드 137(메모리 한도/엔진 장애) 등 오류를 사람이 읽을 수 있게 번역.

검증(로컬 빌드 `rungv v0.16`, macOS arm64/hvf):
- `rungv info/images/volume ls/system df --format json` 출력 필드 확인
- 실행 시 stderr에 `rungv: platform=hvf`, `rungv: publishing 127.0.0.1:18080 -> 80/tcp` 출력
- SIGINT → `rungv run: context canceled` 후 정상 종료

## 2. 기능 분류와 우선순위

| 우선순위 | 영역 | 세부 기능 |
| --- | --- | --- |
| **P0** | 엔진 | `rungv` 자동 탐지(설정 경로·PATH·`~/.gvx/bin`·앱 옆), 버전·백엔드·lockdown·캐시 경로 표시, macOS 서명 진단·원클릭 서명 |
| **P0** | 샌드박스 | 목록(상태·이미지·포트·업타임), 생성 폼(+CLI 미리보기), 시작/중지/재시작, 실시간 로그, 공개 포트 열기, 복제, 삭제, 자동 시작 |
| **P0** | 이미지 | 목록(크기·생성일), Pull(진행 로그), 삭제, Inspect(JSON), 이미지로 바로 실행 |
| **P0** | 볼륨 | 목록(크기·사용 중), 생성, 삭제, Prune, 파일 위치 열기 |
| **P1** | 레시피 | **Docker Engine**(dind + 소켓 + 포트 범위 + 볼륨) 원클릭, `docker context` 생성 안내/실행, **내부 컨테이너 목록·시작/중지/삭제**(Docker API over unix socket) |
| **P1** | 레시피 | **Kubernetes(k3s)** 원클릭, kubeconfig 경로·`KUBECONFIG` 복사, NodePort 범위 안내 |
| **P1** | 스냅샷 | checkpoint 활성 샌드박스 스냅샷 생성(중지/계속 실행), 스냅샷 목록, 복원, 삭제 |
| **P1** | 시스템 | 디스크 사용량(`system df`), 이미지/볼륨 정리 |
| **P1** | 데스크톱 통합 | 트레이 메뉴(실행 중 수, 열기, 모두 중지, 종료), 단일 인스턴스, 창 닫기 → 숨김, 종료 시 샌드박스 정리 |
| **P2** | 고급 | 레지스트리 로그인, save/load, tunables 편집, WireGuard 설정, 대화형 터미널(PTY), 멀티노드 k3s 마법사 |

## 3. 정보 구조와 레이아웃

```
┌──────────────┬─────────────────────────────────────────────────────────┐
│ ◆ gvx        │  Sandboxes                         [Search…] [+ New]    │
│ ● Engine ok  │  3 running · 5 total                                    │
│──────────────│ ┌─────────────────────────────────────────────────────┐ │
│ ▣ Dashboard  │ │ ● web        nginx:alpine   127.0.0.1:8080→80  ▶ ■ ⋯ │ │
│ ▢ Sandboxes 3│ │ ● docker     docker:dind    unix:docker.sock   ▶ ■ ⋯ │ │
│ ≡ Images     │ │ ○ k3s        rancher/k3s    6443               ▶ ■ ⋯ │ │
│ ⛁ Volumes    │ └─────────────────────────────────────────────────────┘ │
│ ◷ Snapshots  │ ┌ Detail: web ───────────────────────────────────────┐ │
│ RECIPES      │ │ [Logs] [Connect] [Config] [Snapshots]              │ │
│ 🐳 Docker    │ │ 2026/11/16 06:24:33 [notice] start worker process… │ │
│ ☸ Kubernetes │ │ …                                                   │ │
│──────────────│ └─────────────────────────────────────────────────────┘ │
│ ⚙ Settings   │                                                         │
├──────────────┴─────────────────────────────────────────────────────────┤
│ rungv v0.16 · hvf · seatbelt │ 3 running │ Images 1.2GB · Volumes 3.4GB │
└────────────────────────────────────────────────────────────────────────┘
```

- **좌측 사이드바**: 엔진 상태 배지(헤더), 주요 목적지, 레시피 섹션, 하단 Settings. `⌘B`로 아이콘 레일 접기.
- **페이지 헤더**: 제목·요약·주요 액션 1개(Primary) + 보조 액션(Outline).
- **목록 + 상세(Master–detail)**: 샌드박스는 테이블에서 선택 → 아래 상세 패널(탭: Logs / Connect / Config / Snapshots). 화면이 좁으면 상세가 전체 폭을 차지.
- **생성은 우측 Sheet**: 페이지 맥락을 유지한 채 폼 작성. 섹션: Image & command → Ports → Storage → Resources → Network & security → Advanced. 하단에 **실시간 `rungv run …` 미리보기**(복사 가능) — GUI가 CLI를 가르치는 구조.
- **하단 상태 바**: 엔진 버전·백엔드·lockdown, 실행 중 수, 디스크 사용량.
- **Dashboard**: 엔진 카드, 요약 지표(실행 중/샌드박스/이미지/볼륨), 레시피 퀵스타트 카드, 최근 활동.

## 4. 핵심 UX 원칙

1. **데몬 없음을 숨기지 않고 활용**: "앱이 실행하는 동안만 샌드박스가 살아있음"을 명시. 창 닫기 = 숨김(트레이 유지), 종료 시 실행 중 샌드박스 확인 후 중지.
2. **시작 전에 확정되는 것들을 폼에서 강조**: 포트/마운트 섹션에 "시작 후 변경 불가" 도움말, 레시피는 범위 포트 기본 제공.
3. **오류는 원인과 해결책으로**: `address already in use` → 포트 충돌 안내, `in use` 볼륨 → 사용 중 샌드박스 이름, 137 → 메모리 한도, 백엔드 없음 → 서명 버튼.
4. **상태는 색+텍스트+아이콘**: Running(초록) / Starting(노랑, 스피너) / Stopped(회색) / Failed(빨강, 종료 코드).
5. **준비 완료 감지**: 레시피별 ready 패턴(`API listen on`, `k3s is up and running`)을 로그에서 감지해 Starting → Running 전환 및 토스트.
6. **연결 정보는 복사 한 번**: Docker context 명령, `KUBECONFIG`, 포트 URL을 복사/열기 버튼으로.
7. **키보드**: `⌘N` 새 샌드박스, `⌘K` 커맨드 팔레트, `⌘B` 사이드바, `⌘1..6` 페이지 이동, `⌘R` 새로고침.

## 5. 아키텍처

```
gvx-desktop (GUI 프로세스, ggui)
 ├─ internal/rungv     rungv CLI 래퍼: 탐지, info/images/volumes/df(JSON), pull/rm/prune, run 인자 생성
 ├─ internal/sandbox   Spec(영속 JSON) · Supervisor(자식 프로세스, 로그 링버퍼, 포트 파싱, ready 감지, 종료 처리)
 ├─ internal/dockerapi Docker Engine API 최소 클라이언트(unix socket) — dind 내부 컨테이너
 ├─ internal/recipes   Docker Engine / Kubernetes 프리셋
 └─ internal/ui        ggui 화면: shell(사이드바·상태바), dashboard, sandboxes, create, images, volumes, snapshots, recipes, settings
        │  (os/exec)
        ▼
   rungv run …  ×N  (샌드박스마다 독립 프로세스, 자체 lockdown)
```

- 백그라운드 작업(프로세스 IO, CLI 호출)은 고루틴에서 수행하고 `App.Post`로 UI 스레드에 결과 반영(ggui 규칙).
- 앱 데이터: `os.UserConfigDir()/gvx-desktop/` — `sandboxes.json`, `settings.json`, `snapshots/<id>/<ts>/`, `run/<id>.sock`.
- `pkg/gvx`를 import하지 않으므로 GUI 빌드는 gvx의 Go 버전 요구(1.27.4+)와 무관.
