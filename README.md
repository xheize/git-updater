# git-updater

`git-updater`는 GitOps 파이프라인(예: ArgoCD, Flux 등)에서 컨테이너 이미지 태그 업데이트를 자동화하기 위한 도구입니다. 이 프로젝트는 이미지 업데이트 요청을 큐(Queue)를 통해 비동기로 처리하는 **API 서버**와, 이 서버에 업데이트 요청을 보낼 수 있는 **CLI 클라이언트**로 구성되어 있습니다.

---

## 주요 기능

1. **비동기 이미지 업데이트**: Webhook 요청을 받으면 내장된 작업 큐(Job Queue)에 등록하고, 단일 워커가 순차적으로 YAML 파일을 수정한 뒤 Git Commit & Push를 수행합니다.
2. **YAML 파싱 및 보존**: `gopkg.in/yaml.v3`를 사용하여 주석(Comments)과 포맷을 유지하면서 특정 이미지 태그만 안전하게 업데이트합니다.
3. **다양한 Git 인증 지원**: SSH Key 인증 및 HTTP Basic(Username/Password/Token) 인증을 모두 지원합니다.
4. **경량화된 웹 프레임워크**: Fiber를 사용하여 가볍고 빠른 API 서버를 제공합니다.

---

## 프로젝트 구조

* **`cmd/server/`**: Webhook 요청을 받고 Git 워커를 구동하는 API 서버 엔트리포인트
* **`cmd/cli/`**: 개발자 PC나 CI 파이프라인에서 API 서버로 업데이트 요청을 쉽게 보낼 수 있는 CLI 도구 엔트리포인트
* **`internal/gitManager/`**: Git Clone, 파일 업데이트, Commit, Push 및 워커 루프 제어를 담당하는 패키지
* **`internal/yaml/`**: YAML 구조를 파싱하고 지정된 경로(`spec.template.spec.containers[0].image` 등)를 업데이트하는 유틸리티 패키지

---

## 환경 변수 설정 (API 서버용)

서버 실행 시 다음 환경 변수를 통해 Git 저장소 접근 권한을 설정해야 합니다.

| 환경 변수명 | 필수 여부 | 설명 |
| :--- | :--- | :--- |
| `GIT_AUTH_METHOD` | **필수** | Git 인증 방식 (`ssh` 또는 `http`) |
| `GIT_SSH_PRIVATE_KEY` | `ssh` 시 필수 | Git 인증에 사용할 SSH Private Key 내용 (String) |
| `GIT_SSH_KNOWN_HOSTS_FILE` | `ssh` 시 **필수** | 원격 Git 서버의 공개 호스트키를 등록한 표준 `known_hosts` 파일 경로 |
| `GIT_USERNAME` | `http` 시 필수 | HTTP Basic 인증용 Username |
| `GIT_PASSWORD` | `http` 시 필수 | HTTP Basic 인증용 Password 또는 Personal Access Token |
| `GIT_REPOSITORY_URL` | **필수** | 업데이트 대상 Git 저장소 주소 (`GIT_REPO_URL`도 호환 지원) |
| `JOB_DB_PATH` | 선택 | 영속 작업 큐 SQLite DB 경로 (기본값: `./data/jobs.db`) |
| `PORT` | 선택 | API 서버가 리스닝할 포트 (기본값: `3000`) |

---

## 실행 및 사용법

### 1. API 서버 구동하기

#### 로컬 실행
```bash
# 환경 변수 설정 예시 (HTTP 인증)
export GIT_AUTH_METHOD="http"
export GIT_USERNAME="your-github-username"
export GIT_PASSWORD="your-personal-access-token"

# 서버 실행
go run ./cmd/server
```

#### Docker로 실행
`Dockerfile`이 이미 작성되어 있으므로 컨테이너로 패키징하여 배포할 수 있습니다.
```bash
# 이미지 빌드
docker build -t git-updater:latest .

# 컨테이너 실행
docker run -d \
  -p 3000:3000 \
	  -v git-updater-data:/app/data \
  -e GIT_AUTH_METHOD="http" \
  -e GIT_USERNAME="username" \
  -e GIT_PASSWORD="token" \
  git-updater:latest
```

#### SSH 인증과 호스트키 검증

SSH를 사용할 때는 개인키뿐 아니라 원격 Git 서버의 호스트키를 포함한 `known_hosts` 파일도 제공해야 합니다. 이 검증은 서버 위조(MITM)를 막기 위한 것으로, 파일에 없는 서버 키나 변경된 키와의 연결은 거부됩니다. 호스트키 지문은 GitHub/GitLab 등 Git 제공자의 공식 문서에서 확인한 뒤 파일에 등록하세요.

```bash
docker run -d \
  -p 3000:3000 \
  -v git-updater-data:/app/data \
  -v /secure/git-private-key:/run/secrets/git-private-key:ro \
  -v /secure/git-known-hosts:/run/secrets/git-known-hosts:ro \
  -e GIT_AUTH_METHOD="ssh" \
	-e GIT_REPOSITORY_URL="git@github.com:your-org/your-repo.git" \
  -e GIT_SSH_PRIVATE_KEY="/run/secrets/git-private-key" \
  -e GIT_SSH_KNOWN_HOSTS_FILE="/run/secrets/git-known-hosts" \
  git-updater:latest
```

---

### 2. CLI 클라이언트로 업데이트 요청하기

개발자 환경이나 CI/CD 툴(Woodpecker, GitHub Actions, GitLab CI 등)에서 API 서버로 업데이트 요청을 직접 보낼 수 있습니다.

#### CLI 빌드
```bash
go build -o git-updater-cli ./cmd/cli
```

#### CLI 실행 옵션
* `-server`: API 서버의 주소 (기본값: `http://localhost:3000`, 또는 `GIT_UPDATER_SERVER_URL` 환경 변수 사용 가능)
* `-file`: 업데이트 대상 YAML 파일 경로 (저장소 내 상대 경로)
* `-image`: 업데이트할 컨테이너 이미지 이름
* `-tag`: 새로 적용할 컨테이너 이미지 태그

#### 사용 예시
```bash
# 명령행 인자를 모두 명시하여 요청 전송
./git-updater-cli \
  -server="http://localhost:3000" \
  -file="deployments/web.yaml" \
  -image="nginx" \
  -tag="1.25.4"

# 환경 변수를 기본값으로 지정하여 간단히 호출
export GIT_UPDATER_SERVER_URL="https://git-updater.your-domain.com"
./git-updater-cli -file="deployments/web.yaml" -image="nginx" -tag="1.25.4"
```

---

## 테스트 실행

프로젝트의 단위 테스트를 진행하려면 아래 명령어를 사용하세요.
```bash
go test -v ./...
```

---

## 작업 상태 및 재시도

요청은 SQLite 작업 저장소에 영속화됩니다. Git 처리 실패 시 5초부터 지수 백오프로 재시도하며, 총 3회 시도 후 `failed` 상태로 남습니다.

* `GET /api/jobs/{jobId}`: 작업 상태, 시도 횟수, 마지막 오류 및 다음 재시도 시각 조회
* `POST /api/jobs/{jobId}/retry`: `failed` 작업을 수동으로 다시 큐에 등록

GitHub webhook은 서버가 현재 추적 중인 브랜치에 대한 `push` 이벤트만 워크스페이스 동기화 작업으로 처리합니다.
