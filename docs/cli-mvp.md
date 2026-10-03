# CLI Change Controller MVP

웹 UI는 구현하지 않았다. CLI/API/webhook이 동일한 Repository Model → ChangeSet → validation → conditional Git publish 경로를 사용한다. 클러스터에 접속하거나 apply하지 않는다.

## 지원 계약

- 서버 하나가 Git 저장소 하나·브랜치 하나를 담당한다. `GIT_TARGET_BRANCH`를 생략하면 clone 시 원격 기본 브랜치를 사용한다.
- `GITOPS_PATH`로 mono-repo의 분석 디렉터리를 제한할 수 있다. 기본값 `.`. 경로는 Git 저장소 기준이며 scope 밖 의존성은 지원하지 않는다. HEAD는 전체 저장소 기준이므로 무관한 파일의 commit도 기존 승인을 무효화한다.
- Kubernetes `v1` Pod/ReplicationController/List, `apps/v1` Deployment/StatefulSet/DaemonSet/ReplicaSet, `batch/v1` Job/CronJob의 알려진 pod-spec 위치에서 containers/initContainers/ephemeralContainers를 읽는다. ConfigMap의 임의 `image` 문자열은 수정하지 않는다. CRD의 사용자 정의 workload 필드는 지원하지 않는다.
- Kustomize는 local `resources`/`bases`, mapping 형태 `images.name/newName/newTag`만 지원한다. 환경 ID는 최상위 Kustomization 디렉터리 경로다. 별도 plain YAML은 `plain:<directory>`로 묶는다. 이는 저장소에서 계산한 프로파일이며 실제 배포 환경을 자동 발견한 결과가 아니다.
- 같은 base를 공유할 때 요청하지 않은 환경까지 변경되면 거절한다. 필요한 override가 없다면 먼저 Git에서 추가해야 한다. override 자동 삽입은 없다.
- Helm, remote bases, patches, replacements, generators, plugins, namespace/name 변환, anchors/aliases/merge keys, Git submodules 등은 지원하지 않는다. 선택 scope에 parser diagnostic이 있으면 **전체 ChangeSet을 거절**한다. 지원 불가 구문을 일반 YAML로 우회하지 않는다.
- 이미 digest로 고정된 참조는 tag 변경으로 풀지 않는다. digest intent와 Helm values binding은 후속 범위다.
- 기존 scalar의 byte span만 바꾸고 주석·나머지 공백·key order·CRLF·quote를 보존한다. 새 tag가 숫자/boolean으로 읽힐 수 있으면 그 scalar만 quote한다. 안전하게 위치를 확인할 수 없는 scalar는 거절한다. preview는 전체 파일의 unified hunk를 표시하지만 실제 commit의 변경은 scalar 교체다.
- YAML 한 파일 2 MiB, scope 내 YAML 합계 32 MiB/10,000개 및 dependency traversal 제한이 있다. 이 MVP의 검증은 Kubernetes 전체 schema/admission validation이나 native Helm/Kustomize의 모든 동작 검증을 대체하지 않는다.

## 서버 설정

기존 Git 인증, `API_KEY`, `JOB_DB_PATH`, 작성자 설정을 유지한다. 추가 설정:

| 변수 | 용도 |
|---|---|
| `GIT_TARGET_BRANCH` | 선택할 기존 브랜치. 기존 workspace의 브랜치와 다르면 시작 거절 |
| `GITOPS_PATH` | 분석 scope. 기본 `.` |
| `GITHUB_TOKEN` | GitHub REST repository/read/write/branch 검증용 token. HTTP Git 인증에서는 생략 시 `GIT_PASSWORD` 사용. SSH 사용 시 별도로 설정 |
| `GITHUB_REPOSITORY_ID` | 권장 identity pin. webhook 사용 시 기존처럼 필수. 조회한 ID와도 비교 |
| `REGISTRY_HOSTS` | 검증을 허용할 registry hostname[:port] 목록. 쉼표로 구분. Docker Hub 이름은 `docker.io` |
| `REGISTRY_AUTH_HOST` | 아래 인증을 보낼 단일 registry host. allowlist에도 있어야 함 |
| `REGISTRY_USERNAME`, `REGISTRY_PASSWORD` | 해당 registry의 Basic 인증 |
| `REGISTRY_BEARER_TOKEN` | 해당 registry의 Bearer token. Basic보다 우선 |
| `AUTOMATION_ENVIRONMENTS` | 자동 변경을 허용할 환경 ID 목록. 생략하면 전체. 명시적 CLI apply/기존 Force 요청은 수동 변경으로 처리 |
| `CONTROLLER_LOCAL_MODE` | 기본 비활성. `true`는 절대 로컬 파일 경로의 Git origin만 허용하며 provider/registry 검증을 생략하는 테스트 모드. 네트워크 origin에는 사용할 수 없음 |

운영 provider는 현재 **github.com만** 지원한다. GitHub API에서 immutable ID, 접근 권한, archived/disabled 및 branch protection을 확인한다. protected branch에는 직접 게시하지 않으며 PR workflow는 미구현이다. REST에서 보이지 않는 추가 Git 규칙은 최종 push에서도 거절될 수 있다. 최초 확인한 provider ID와 branch를 SQLite에 고정해 다른 저장소를 같은 DB로 변경하는 것을 차단한다. rename 이후 URL 설정과 workspace 교체는 운영자가 수행해야 하며 같은 ID/branch의 DB 기록은 유지할 수 있다.

Registry는 HTTPS manifest HEAD로 tag 존재와 digest를 확인한다. **override 이후 실제 effective image 이름**을 검증한다. preview와 apply에서 digest가 달라지면 거절한다. 자동 Bearer challenge/token 교환 및 Docker credential-helper 연동은 없다. 필요한 registry token을 운영자가 제공해야 한다. 태그 자체는 mutable하므로 마지막 확인 이후 tag 이동까지 방지하는 보장은 없으며, 재현성 강화를 위한 digest pinning은 별도 기능이다.

GitHub와 registry 검증에 사용하는 자격 증명은 서버에만 둔다. CLI에는 controller API key만 제공한다.

기존 버전에서 업그레이드할 때는 발신을 잠시 멈추고 pending/retrying 작업을 검토한다. 새 테이블은 기존 작업 이력을 보존하지만, 과거 작업의 base revision을 소급해서 알아낼 수는 없다. 새 worker의 첫 시도부터 baseline을 고정한다. 임의 YAML의 `image` key나 미지원 Helm/Kustomize를 바꾸던 이전 동작은 더 이상 허용하지 않는다.

## 기본 사용 흐름

PowerShell 예시:

```powershell
go build -o git-updater.exe ./cmd/server
go build -o git-updater-cli.exe ./cmd/cli

# 서버 프로세스 환경: 실제 값은 secret 주입으로 설정한다.
$env:API_KEY = '<controller-api-key>'
$env:GIT_AUTH_METHOD = 'http'
$env:GIT_USERNAME = '<github-user>'
$env:GIT_PASSWORD = '<github-token>'
$env:GIT_REPOSITORY_URL = 'https://github.com/<owner>/<gitops-repo>.git'
$env:GITHUB_REPOSITORY_ID = '<numeric-repository-id>'
$env:GIT_TARGET_BRANCH = 'main'
$env:GITOPS_PATH = 'apps'
$env:REGISTRY_HOSTS = 'registry.example.com'
$env:REGISTRY_AUTH_HOST = 'registry.example.com'
$env:REGISTRY_USERNAME = '<registry-reader>'
$env:REGISTRY_PASSWORD = '<registry-password>'
./git-updater.exe
```

서버와 별도 터미널에서:

```powershell
$env:GIT_UPDATER_SERVER_URL = 'http://localhost:3000'
$env:GIT_UPDATER_API_KEY = '<controller-api-key>'

./git-updater-cli.exe inspect
./git-updater-cli.exe images --image registry.example.com/backend

./git-updater-cli.exe plan --id release-20260927 `
  --change registry.example.com/backend=v1.9.0 `
  --change registry.example.com/frontend=v3.5.0

# plan 출력의 base revision, 영향 범위, artifact digest, diff를 검토한 후:
./git-updater-cli.exe apply --id release-20260927
./git-updater-cli.exe show --id release-20260927
./git-updater-cli.exe changesets --limit 20 --offset 0
./git-updater-cli.exe jobs --limit 20 --offset 0
```

`plan`은 commit/push하지 않는다. ID를 생략하면 CLI가 생성한다. 동일 ID·동일 intent는 저장된 계획을 반환하며 최신 HEAD에서 다시 계산하지 않는다. 같은 ID로 요청 내용을 바꾸면 409다. HEAD나 scope가 바뀌면 **새 ID로 plan을 만들고 다시 검토**해야 한다. `apply`는 저장된 계획 ID만 받아 실행하며 클라이언트가 전송한 diff를 실행하지 않는다.

환경/파일 선택:

```powershell
./git-updater-cli.exe plan --image registry.example.com/backend --tag v2 `
  --env apps/backend/overlays/dev,apps/backend/overlays/staging
./git-updater-cli.exe plan --image registry.example.com/backend --tag v2 `
  --file apps/backend/overlays/dev/kustomization.yaml
```

복잡한 요청은 `plan --intent release.json`, 자동화 출력은 모든 명령에 `--json`을 사용한다.

```json
{
  "id": "release-20260927",
  "changes": [
    {"image": "registry.example.com/backend", "tag": "v1.9.0", "environments": ["apps/backend/overlays/dev"]},
    {"image": "registry.example.com/frontend", "tag": "v3.5.0", "environments": ["apps/frontend/overlays/dev"]}
  ]
}
```

`job --id <jobId>`와 `retry --id <jobId>`도 제공한다. 기존 `-image/-tag` CLI 형식과 `/api/update`, `/webhook`은 호환을 위해 즉시 실행 요청으로 유지하지만 같은 planner/validation/CAS 경로를 탄다. 작업 ID에 대응하는 ChangeSet은 `job-<sha256(jobId)>`이며 `changesets`에서 확인할 수 있다. 자동 webhook에는 `AUTO_UPDATE=true`가 필요하다.

## API

모든 아래 경로는 기존 Bearer 또는 X-API-Key 인증을 요구한다. `/health`, `/ready`의 기존 의미는 그대로다.

| Method/path | 동작 |
|---|---|
| `GET /api/repository` | 현재 revision의 Model, identity/access 상태, branch, scope |
| `POST /api/changesets` | Intent JSON → 검증된 영속 preview. 200은 게시 완료가 아님 |
| `GET /api/changesets?limit=20&offset=0` | ChangeSet 이력 |
| `GET /api/changesets/:id` | 승인 대상 diff/impact 및 결과 |
| `POST /api/changesets/:id/apply` | 해당 계획을 동기 실행. 이미 게시된 계획은 기존 결과 반환 |
| `GET /api/jobs?limit=20&offset=0` | 기존 비동기 작업 목록 |
| `GET /api/jobs/:id`, `POST /api/jobs/:id/retry` | 기존 작업 상세/재시도 |

잘못된 JSON/pagination 400, 인증 실패 401, 미존재 404, stale/ID 충돌 409, 계획 validation 실패 422, 내부 작업 불가 503. `apply` 응답 손실 시 새 intent를 보내기 전에 기존 ID를 조회한다.

## 동시성·복구

- 같은 프로세스의 snapshot/preview/apply/webhook sync는 공통 mutex로 직렬화한다. DB와 workspace에 OS 파일 lock을 잡아 동일 상태를 공유하는 중복 서버 시작을 차단한다. lock 파일은 남아 있어도 프로세스 종료 시 OS lock은 해제된다. 분산 lease나 여러 replica 운영을 지원하는 것은 아니다.
- base revision을 다시 확인한 뒤 변경 파일 전체를 한 commit으로 만들고, 원격 ref에 `RequireRemoteRefs=baseRevision:targetRef` 조건을 걸어 push한다. 기본 fast-forward 검사에만 의존하지 않는다. 여러 저장소를 묶는 atomicity는 제공하지 않는다.
- `planned → publishing → published` 상태와 후보 commit OID를 push 전에 영속화한다. push 응답 손실은 `unknown`으로 남긴다. 재호출은 현재 원격 이력에서 후보 commit이 확인될 때만 published로 회복한다. 최대 10,000 commits 안에서 입증하지 못하면 자동 재게시하지 않는다.
- worker의 실패 전 base도 영속화한다. 저장소가 달라진 뒤의 오래된 retry는 `failed/conflict`로 끝나며 새 HEAD에서 조용히 재계획하지 않는다. 이미 conflict인 작업은 retry API도 409를 반환한다. 이미지 버전 순서나 늦게 도착한 **새** webhook의 의미적 역행까지 판별하는 정책은 아직 없다.
- `already_satisfied`는 변경할 내용이 없어 commit이 없는 결과다. 기존 published 결과를 다시 조회하는 것은 현재 HEAD가 여전히 그 상태라는 뜻이 아니다.
- workspace origin 불일치나 sync 실패에서 workspace를 자동 삭제하지 않는다. DB/branch 또는 저장소를 바꾸려면 별도의 작업 디렉터리와 DB를 사용한다.

## 검증과 남은 범위

실제 서버/CLI/TCP HTTP/SQLite/local Git E2E와 단위·회귀 테스트로 다음을 검증한다: 복수 이미지 한 commit, preview 무변경, override provenance와 공유 base 영향 차단, ConfigMap 오탐 배제, CRLF/Unicode/quote 보존, 숫자 tag quoting, 잘못된 원소 전체 거절, stale HEAD/remote rewind/branch 삭제, 동일 계획 동시 apply, duplicate/restart/push 결과 복구, 오래된 retry 거절, workspace/DB lock 및 identity mismatch.

GitHub/registry 검증은 mock HTTP transport로 확인했다. **실제 운영 token, provider branch rules, 사설 registry 인증, 원격 CI/배포는 별도 실환경 검증이 필요하다.** 로컬 E2E는 `CONTROLLER_LOCAL_MODE=true`를 명시해 외부 서비스에 접근하지 않는다.

2026-09-27: E2E를 포함한 전체 테스트와 `go vet ./...`, `git diff --check`가 통과했다. Linux amd64 `CGO_ENABLED=0` 서버·CLI cross-build도 통과했다. Linux 실행 및 실제 컨테이너/클러스터 배포를 검증한 결과는 아니다.

```powershell
$env:RUN_API_E2E = '1'
go test -buildvcs=false ./... -count=1 -timeout 150s
go vet ./...
```

후속 범위는 웹 UI, Helm explicit bindings, 전체 Kustomize/native render 검증, PR/approval 역할 분리, GitLab/Enterprise providers, registry token negotiation, digest mutation, multi-repo release 및 HA다.

검증 프로토콜 근거: [GitHub repository API](https://docs.github.com/en/rest/repos/repos#get-a-repository), [OCI manifest 존재 확인](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#checking-if-content-exists-in-the-registry), [Kustomize image transformation](https://github.com/kubernetes-sigs/kustomize/blob/master/api/filters/imagetag/updater.go).

## Zot CloudEvents

`POST /webhook/zot` accepts authenticated CloudEvents 1.0 in binary HTTP mode
(`ce-specversion`, `ce-id`, `ce-source`, `ce-type`, optional `ce-time`) or
structured mode (`Content-Type: application/cloudevents+json`). Zot 2.x
`zotregistry.image.updated` events map `data.name` and `data.reference` to an
image/tag update. Non-update events and digest-addressed uploads are ignored.
Malformed update events return 400; missing API authentication returns 401.
The legacy `action: push` / `target` JSON format remains supported.

Set `ZOT_REGISTRY_HOST` to the registry hostname, optionally including its port,
without a URL scheme or path. When unset, a single `REGISTRY_HOSTS` entry is used.
Multiple allowed registries require an explicit `ZOT_REGISTRY_HOST`. CloudEvents
`source` (e.g. `zotregistry.dev`) identifies the producer, not the image registry.
A single endpoint configuration currently represents one Zot registry.

Delivery IDs are scoped by configured registry and event source. Binary and
structured encodings of the same event share a persisted job; reusing an ID
with changed data/time returns 409. A 202 response means durable admission,
not successful Git publication. AUTO_UPDATE, repository identity, parser,
registry validation and Git concurrency checks still apply. HTTP 200 ignored
must not be interpreted as a successful image update.
