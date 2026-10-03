# git-updater

GitOps 저장소를 해석하고 변경 영향을 검증한 뒤, 다음 desired state를 Git commit으로 만드는 **CLI 중심 GitOps Change Controller**입니다.

```text
CLI / API / Zot webhook
  → Repository Model · 이미지 사용처 해석
  → ChangeSet · 영향 분석 · 검증 · diff
  → Git commit / 조건부 push
  → Argo CD 등 downstream GitOps controller
  → Cluster
```

서버는 Kubernetes API에 접근하거나 리소스를 apply하지 않습니다. Kubernetes는 선택 가능한 실행 환경이며, VM이나 Docker에서도 사용할 수 있습니다. CLI는 서버의 HTTP API를 호출하는 클라이언트입니다. 웹 UI는 없습니다.

## 현재 지원 범위

| 항목 | 구현 범위 |
|---|---|
| 저장소 | 서버 하나당 Git 저장소 하나·브랜치 하나, `GITOPS_PATH`로 분석 범위 지정 |
| 파서 | 알려진 Kubernetes workload의 이미지, 제한된 local Kustomize `resources` / `bases` / `images` |
| 변경 계획 | source/effective image 구분, 수정 위치·영향 환경·diff preview |
| ChangeSet | 여러 이미지 변경을 하나의 계획과 Git commit으로 묶기 |
| YAML 수정 | 원문 scalar 위치만 변경하여 나머지 주석·공백·key order 보존 |
| 검증 | GitHub repository identity·접근·브랜치 보호, registry tag·digest |
| 동시성 | revision 고정, 원격 ref 조건부 push, stale 계획 거절 |
| 작업 관리 | SQLite 영속 큐·계획·이력, 중복 이벤트 방지, 실패 재시도 |
| 입력 | CLI/API, Zot CloudEvents 및 기존 Zot JSON, GitHub push 동기화 webhook |

**미지원:** Helm, remote Kustomize bases, patches/generators/plugins 등 복잡한 변환, 이미지별·레포별 rule engine, PR 생성 workflow, 여러 저장소를 묶는 atomic commit, 다중 서버 운영. 선택 범위에 parser diagnostic이 있으면 변경 계획을 거절합니다. 전체 지원 계약은 [CLI MVP 문서](docs/cli-mvp.md)를 참고하세요.

## 빠른 시작

### 서버 실행

Go 버전은 [go.mod](go.mod)를 따릅니다. 다음은 Bash 예시입니다. `<...>`는 실제 환경에 맞게 바꾸고, 인증정보는 Secret 관리 도구나 별도 환경 파일에서 주입하세요.

```bash
go build -o git-updater ./cmd/server
go build -o git-updater-cli ./cmd/cli

export API_KEY='<controller-api-key>'
export GIT_REPOSITORY_URL='https://github.com/<owner>/<gitops-repo>.git'
export GIT_AUTH_METHOD='http'
export GIT_USERNAME='<github-user>'
export GIT_PASSWORD='<github-token>'
export GITHUB_TOKEN='<github-api-token>'
export GIT_TARGET_BRANCH='main'
export GITOPS_PATH='apps'
export REGISTRY_HOSTS='registry.example.com'
export AUTO_UPDATE='false'

./git-updater
```

`apps`는 실제 저장소에 존재하는 경로로 바꾸세요. 사설 registry는 아래 인증 설정도 필요합니다. 초기에는 조회와 preview로 범위를 확인하고 자동 변경을 활성화하세요.

### CLI로 조회 → 계획 → 적용

별도 터미널에서 실행합니다. CLI에는 GitHub/registry 인증정보 대신 서버 API key만 제공합니다.

```bash
export GIT_UPDATER_SERVER_URL='http://localhost:3000'
export GIT_UPDATER_API_KEY='<controller-api-key>'

./git-updater-cli inspect
./git-updater-cli images --image registry.example.com/backend

./git-updater-cli plan --id release-example \
  --change registry.example.com/backend=v1.9.0 \
  --change registry.example.com/frontend=v3.5.0

# 표시된 base revision, 영향 범위, 검증 결과와 diff를 검토한 뒤 실행
./git-updater-cli apply --id release-example
./git-updater-cli show --id release-example
./git-updater-cli changesets
./git-updater-cli jobs
```

`plan`은 commit/push하지 않습니다. `apply`는 저장된 계획을 검증한 뒤 게시합니다. HEAD가 바뀌면 새 ID로 계획을 다시 만들어야 합니다. 같은 ID를 다른 요청 내용으로 재사용하면 409입니다. 기계가 읽을 출력은 `--json`, 환경·파일 선택은 `plan --env` / `--file`을 사용하세요.

기존 `-image/-tag` CLI도 지원하지만 즉시 실행 요청입니다. 변경 전 검토에는 `plan/apply`를 사용하세요.

## 인증과 서버 설정

세 가지 인증은 역할이 다릅니다.

| 인증 | 용도 |
|---|---|
| `API_KEY` | CLI/API/Zot → git-updater 접근 |
| SSH 키 또는 HTTP Git 인증 | git-updater → Git clone/fetch/push |
| `GITHUB_TOKEN` | git-updater → GitHub API의 repository ID·권한·브랜치 검증 |

**SSH가 동작해도 GitHub API 검증에는 token이 필요합니다.** 현재 token 입력은 환경변수입니다. `GITHUB_TOKEN_FILE`이나 GitHub App token 자동 발급은 아직 구현하지 않았습니다. Kubernetes에서는 Secret의 값을 환경변수로 주입할 수 있습니다. 실제 인증정보를 Git에 커밋하지 마세요.

### 기본·Git 설정

| 변수 | 의미 |
|---|---|
| `API_KEY` | 필수. `WEBHOOK_SECRET` 호환 지원 |
| `GIT_REPOSITORY_URL` | 대상 저장소. `GIT_REPO_URL` 호환 지원 |
| `GIT_AUTH_METHOD` | `http` 또는 `ssh` |
| `GIT_USERNAME`, `GIT_PASSWORD` | HTTP Git 인증 시 필수 |
| `GIT_SSH_PRIVATE_KEY` | SSH 개인키 내용 또는 파일 경로 |
| `GIT_SSH_KNOWN_HOSTS_FILE` | SSH 사용 시 필수. 검증한 Git 서버 호스트키 파일 |
| `GITHUB_TOKEN` | GitHub API 검증용. HTTP Git 모드에서는 생략 시 `GIT_PASSWORD` 사용 |
| `GITHUB_REPOSITORY_ID` | 권장 identity pin. GitHub webhook 활성화 시 필수 |
| `GIT_TARGET_BRANCH` | 대상 기존 브랜치. 생략 시 clone의 기본 브랜치 |
| `GITOPS_PATH` | 저장소 기준 분석 디렉터리. 기본 `.` |
| `JOB_DB_PATH` | SQLite 경로. 기본 `./data/jobs.db` |
| `PORT` | 기본 `3000` |
| `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL` | 기본 `git-updater`, `git-updater@localhost` |

운영 provider 검증은 현재 github.com만 지원하며, protected branch에는 직접 push하지 않습니다. Git URL을 바꾸는 것만으로 동일 DB를 다른 repository identity에 재사용할 수 없습니다.

### Registry 설정

| 변수 | 의미 |
|---|---|
| `REGISTRY_HOSTS` | tag 검증을 허용할 hostname[:port] 목록. 쉼표로 구분 |
| `REGISTRY_AUTH_HOST` | 아래 인증을 보낼 단일 host. allowlist에도 있어야 함 |
| `REGISTRY_USERNAME`, `REGISTRY_PASSWORD` | 해당 registry의 Basic 인증 |
| `REGISTRY_BEARER_TOKEN` | 해당 registry의 Bearer 인증. Basic보다 우선 |
| `ZOT_REGISTRY_HOST` | Zot 이벤트의 이미지 이름에 붙일 registry host. 생략 시 단일 `REGISTRY_HOSTS` 사용 |

Pod의 `imagePullSecret`은 서버 내부 registry API 인증을 대신하지 않습니다. registry token의 자동 challenge 교환은 미지원입니다. 태그는 변경 가능한 이름이므로 digest 확인이 이미지의 영구 불변성을 보장하지는 않습니다.

## 자동 변경과 정책

- `AUTO_UPDATE=true`: webhook 등 자동 이미지 변경 작업을 허용합니다. 기본값은 비활성입니다.
- `AUTOMATION_ENVIRONMENTS`: 자동 변경 허용 환경 ID 목록입니다. 쉼표로 구분하며 생략하면 환경 제한이 없습니다.
- 명시적 `apply`와 기존 `Force` 요청은 수동 경로입니다. 자동 처리 스위치·환경 필터와 별개이지만 repository/registry 검증과 CAS는 수행합니다.
- 공통 base 수정이 선택하지 않은 환경까지 바꾸면 거절합니다.

현재는 **전역 스위치와 공통 환경 필터**만 있습니다. 글로벌/레포/이미지별 rule, 우선순위 병합, 태그 패턴·SemVer 선택은 아직 없습니다. `AUTO_UPDATE=true`만으로 인증·파서·registry 검증 실패가 해소되지는 않습니다.

## Webhook

### Zot → `/webhook/zot`

CLI를 거치지 않고 Zot가 서버로 직접 POST합니다. 인증은 `Authorization: Bearer <API_KEY>` 또는 `X-API-Key: <API_KEY>`입니다.

Zot HTTP sink 예시입니다. 아래 값은 자리표시자이며, 실제 Authorization 값이 들어간 설정은 Secret으로 관리하세요. 이 JSON이 환경변수를 자동 치환한다는 의미는 아닙니다.

```json
{
  "type": "http",
  "address": "http://git-updater-service.gitupdate.svc.cluster.local/webhook/zot",
  "timeout": "5s",
  "headers": {
    "Authorization": "Bearer <controller-api-key>"
  }
}
```

위 주소는 배포 예제의 Kubernetes Service 기준입니다. 실제 Zot workload가 마운트한 설정을 확인하세요. 이름이 같은 ConfigMap과 Secret이 있어도 사용되는 것은 마운트 대상뿐입니다. `subPath`로 마운트한 설정은 변경 후 Pod 재생성이 필요합니다.

지원하는 CloudEvents 형식:

- **Binary HTTP:** `ce-specversion: 1.0`, `ce-id`, `ce-source`, `ce-type`, 선택적 `ce-time` 헤더와 JSON data body
- **Structured JSON:** `Content-Type: application/cloudevents+json`과 metadata/data envelope
- `zotregistry.image.updated`의 `name` / `reference`를 이미지·태그로 해석
- 다른 이벤트와 digest 주소로 업로드된 manifest는 무시
- 같은 registry·source·event ID는 중복 작업을 만들지 않으며, 내용이 달라지면 409

CloudEvents의 `source`인 `zotregistry.dev`는 registry 주소가 아닙니다. `ZOT_REGISTRY_HOST`를 사용하거나 단일 `REGISTRY_HOSTS`를 설정해야 합니다. 기존 `action: push` / `target` JSON도 지원합니다.

**202는 작업 영속 저장, 200 `ignored`는 이벤트 제외를 의미합니다. 둘 다 Git 변경 성공을 뜻하지 않습니다.** 반환된 `jobId`로 작업 결과를 확인하세요.

### GitHub → `/webhook/github`

`GITHUB_WEBHOOK_SECRET`과 `GITHUB_REPOSITORY_ID`를 설정합니다. `GITHUB_WEBHOOK_ENABLED=false`로 비활성화할 수 있습니다. 서명과 repository ID를 확인한 뒤 대상 브랜치의 push를 Git 동기화 작업으로 처리합니다. 이 이벤트 자체가 이미지 버전을 선택하지는 않습니다.

## Docker와 Kubernetes

현재 기본 이미지는 서버 `/app/git-updater`와 CLI `/app/git-updater-cli`를 모두 포함합니다. CLI는 호출할 때만 실행되며 별도 상시 프로세스는 아닙니다. 서버/CLI 이미지 분리는 현재 main에 포함되지 않았습니다.

```bash
docker build -t git-updater:local .
# /secure/server.env에 위 서버 설정을 준비하고 접근 권한을 제한합니다.
docker run -d --name git-updater \
  -p 127.0.0.1:3000:3000 \
  --env-file /secure/server.env \
  -v git-updater-data:/app/data \
  git-updater:local

docker exec git-updater sh -c \
  'GIT_UPDATER_API_KEY="$API_KEY" /app/git-updater-cli inspect'
```

Kubernetes 배포·Secret·PVC·업그레이드는 [k3s 운영 문서](deploy/k3s/README.md)를 참고하세요. 단일 Pod와 `Recreate`, SQLite용 PVC를 사용합니다. 기존 GitOps 관리 리소스는 Git에서 수정하여 Argo CD 등이 반영하도록 합니다.

```bash
kubectl -n gitupdate port-forward service/git-updater-service 3000:80
# 별도 터미널에서 로컬 CLI 사용, 또는 아래처럼 포함된 CLI 실행
kubectl -n gitupdate exec deployment/git-updater -- sh -c \
  'GIT_UPDATER_API_KEY="$API_KEY" /app/git-updater-cli inspect'
```

CI는 main push에서 `nightly-<sha8>` / `nightly`, Git tag 이벤트에서 해당 버전 태그 / `latest` 이미지를 발행하도록 설정되어 있습니다. 배포 manifest의 이미지 변경은 별도 단계입니다. 자세한 조건은 [.woodpecker.yaml](.woodpecker.yaml)을 참고하세요.

## 상태 확인과 장애 대응

| 확인 경로 | 의미 |
|---|---|
| `GET /health`, `GET /ready` | 프로세스·워커·DB 상태. Git 쓰기 성공 보장은 아님 |
| `GET /api/repository` 또는 `inspect` | identity 검증 상태, revision, 사용처, parser diagnostic |
| `GET /api/status` | 비동기 작업 집계 |
| `jobs`, `job --id <jobId>` | webhook/기존 API의 작업·시도 횟수·오류 |
| `changesets`, `show --id <planId>` | 변경 계획·diff·게시 결과 |

`/api/*`는 API key 인증이 필요합니다. 비동기 Git 처리 실패는 최대 3회 시도하며, 실패 원인을 해소한 뒤 `retry --id <jobId>`를 사용할 수 있습니다. baseline HEAD가 달라진 작업은 새 요청이 필요합니다. `apply`는 저장된 계획을 동기 실행하는 별도 API입니다.

| 증상 | 확인할 항목 |
|---|---|
| Zot POST 404 | `/webhook/zot` 주소와 실제 마운트 설정 |
| Zot POST 401 | 서버 API key와 sink 인증 일치 여부 |
| CloudEvent 400 | 필수 metadata/data, registry host 설정 |
| `GITHUB_TOKEN is required` | SSH Git 인증과 별개인 GitHub API token |
| `repository has parser diagnostics` | `inspect` 결과와 미지원 구문, 분석 scope |
| registry 검증 실패 | tag 존재, allowlist, 서버용 registry 인증 |
| stale/plan conflict | 최신 HEAD에서 새 ID로 계획 생성 |

서버 장애·작업 실패의 외부 알림 연동은 아직 없습니다. 상태 API와 로그를 운영 모니터링에 연결해야 합니다.

## 개발 및 검증

```bash
go test ./... -count=1
go vet ./...
RUN_API_E2E=1 go test ./... -count=1 -timeout 150s
```

PowerShell에서는 `$env:RUN_API_E2E='1'`로 설정한 뒤 테스트합니다. E2E는 임시 로컬 Git 저장소와 실제 서버/CLI 프로세스로 수행합니다. `CONTROLLER_LOCAL_MODE=true`는 절대 로컬 Git 경로만 허용하는 테스트 옵션이며 운영 인증 우회용으로 사용할 수 없습니다.

| 경로 | 책임 |
|---|---|
| `cmd/server` | HTTP API, 인증, webhook 정규화 |
| `cmd/cli` | 서버 API를 사용하는 CLI |
| `internal/controller` | Repository Model, resolver, 영향 분석, 최소 mutation 계획 |
| `internal/gitManager` | Git/provider/registry 검증, SQLite, worker, 조건부 push |
| `internal/singlewriter` | workspace·DB 중복 실행 방지 |

상세 계약과 API 목록: [CLI MVP](docs/cli-mvp.md) · 배포 절차: [k3s 운영](deploy/k3s/README.md)
