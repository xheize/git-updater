# API 테스트 및 점검 결과

점검일: 2026-09-27. production 코드 기준: `6ec8a6a` (문서 변경 이후 실행 코드는 동일). 이번 변경은 테스트 추가와 점검 기록이며 아래 발견 사항의 수정은 포함하지 않는다.

## 1. 결과

정상 API 처리와 로컬 Git 반영 경로는 통과했다. 그러나 입력 의미 검증, 동일 ID의 다른 payload 처리, 결과 상태 구분에 결함이 있어 **테스트 통과를 안전한 자동 변경 controller 완성으로 해석하면 안 된다.**

| 검증 | 결과 / 범위 |
|---|---|
| 전체 회귀 테스트 | `go test -buildvcs=false ./... -count=1` 통과 |
| 정적 검사 | `go vet ./...` 통과 |
| API 계약 테스트 | 인증, validation 오류, 영속 admission, 중복, 동시 요청, GitHub/Zot, DB 장애의 7개 테스트 그룹 추가·통과 |
| 동시 요청 | 24개 요청: 서로 다른 ID 12개 + 동일 ID 12개 → pending 13개, 유실 없이 영속화 |
| 실제 프로세스 E2E | 서버와 CLI 빌드·실행, TCP HTTP, SQLite, local bare Git commit/push 통과 |
| 재시도 | 파일 부재 시 3회 시도 후 failed, fixture 보완 후 수동 retry로 성공 |
| 재시작 | 테스트 소유 서버 프로세스 종료·재시작 후 완료 작업 이력 유지 |
| 커버리지 | `cmd/server` statement 78.6%; 별도 subprocess 바이너리 실행은 이 수치에 포함되지 않음 |
| 외부 시스템 | 실제 GitHub/GitLab/Zot/registry, 실제 운영 도메인·TLS·배포 환경은 미검증 |

Windows의 기본 GOCACHE 및 임시 Git clone 접근은 샌드박스 제약이 있어 writable cache를 사용했다. Git을 사용하는 전체 테스트와 process E2E는 일반 권한으로 실행했다. 실제 사용자 저장소나 외부 Git remote에는 테스트 commit/push를 보내지 않았다.

## 2. 추가한 테스트

### `cmd/server/api_contract_test.go`

실제 SQLite를 쓰는 Fiber handler 테스트다. worker는 실행하지 않으므로 `202`가 처리 완료가 아닌 durable admission임을 검증한다.

- 보호된 6개 endpoint에 인증 누락·틀린 token → 401, DB 작업 없음.
- Bearer 및 X-API-Key 정상 인증.
- 잘못된 JSON, 필드 누락, 타입 오류, 잘못된 timestamp → 400, DB 작업 없음.
- `/api/update`, `/webhook`의 작업 영속화, ID/timestamp 생성, 요청 action을 update로 고정.
- 동일 ID·동일 payload 재전송 시 한 job·한 wakeup.
- notification channel이 가득 차도 SQLite에 작업 보존.
- 24개 동시 admission에서 개별 요청 ID와 DB 레코드 확인.
- jobs 조회 200/404, pending 작업 retry 409, 미존재 작업 retry 404.
- GitHub HMAC 누락/위조/본문 변경 거절, 정상 서명이어도 다른 branch/event 무시.
- GitHub 중복 delivery는 한 sync job; image/tag 입력을 update로 해석하지 않음.
- Zot malformed/missing fields 거절, pull/delete 무시, host/port 매핑 및 중복 억제.
- DB close 시 enqueue 500·status 503·조회 500, 저장되지 않은 작업은 queue에 보내지 않음.

### `cmd/server/api_process_test.go`

`RUN_API_E2E=1`일 때만 실행한다. Go와 native Git이 필요하며 전체 리소스는 `t.TempDir()`에 생성한다. 서버는 임시 포트에 시작하고 테스트 종료 시 자신이 만든 자식 프로세스만 종료한다.

1. local bare origin과 seed 저장소를 만들고 image를 공유하는 Deployment 두 개를 seed한다.
2. 실제 서버의 health/readiness, 인증 오류, 잘못된 JSON, 미존재 job 응답을 확인한다.
3. API 요청으로 두 파일이 **한 새 commit**에 갱신됐는지 origin tree와 parent OID를 확인한다.
4. 동일 job 중복 요청 및 별도 job의 이미 반영된 값 요청이 추가 commit을 만들지 않는지 확인한다.
5. 실제 CLI와 Zot payload로 이미지가 원격 tree에 반영되는지 확인한다.
6. GitHub sync job은 성공하되 새 commit을 만들지 않는지 확인한다.
7. 없는 파일에 대한 작업이 자동 3회 재시도 뒤 failed가 되는지 확인한다.
8. origin에 해당 파일을 추가하고 retry API로 다시 요청해 성공·push까지 확인한다.
9. 서버를 종료·재시작하고 이전 완료 작업 조회를 확인한다.

이 테스트의 restart는 프로세스 강제 종료 이후 DB 보존 확인이다. graceful SIGTERM, push 진행 중 crash, distributed worker recovery를 검증한 것은 아니다. 24개 동시 요청 테스트도 **admission 동시성**이며 여러 Git writer 간 CAS 검증은 아니다.

## 3. 발견 사항

### P1 — 잘못된 image/tag를 저장하고 실제 Git에 반영

`handleJobEnqueue()`는 `Image == "" || Tag == ""`만 확인한다. 공백 image/tag와 공백이 포함된 tag가 202로 영속화됐다.

```json
{"id":"invalid-tag","image":"registry.test/demo/api","tag":"bad tag"}
```

임시 E2E overlay로 위 요청을 실제 서버에 보냈다. 결과는 `succeeded`였고 origin의 Deployment에 `image: registry.test/demo/api:bad tag`가 기록됐다. 이는 유효한 YAML scalar이지만 유효한 container image tag는 아니다.

수정 방향: admission에서 image reference/tag/digest 문법을 검증하고 control/whitespace·모호한 조합을 거절한다. image reference 전용 parser를 사용하고 JSON 파싱 성공과 이미지 유효성을 구분한다. registry artifact 존재 검증은 별도의 후속 계층이다.

### P1 — 동일 ID·다른 payload가 오류 없이 사라짐

```text
POST /api/update  id=collision, image=nginx, tag=v1 → 202
POST /api/update  id=collision, image=nginx, tag=v2 → 202
GET  /api/jobs/collision                            → tag=v1
```

`JobStore.Enqueue()`의 `ON CONFLICT(id) DO NOTHING` 때문에 두 번째 intent가 반영되지 않는데도 accepted 응답을 준다. 같은 ID로 Zot의 busybox:v9 요청을 보내도 202이며 저장된 내용은 nginx:v1이었다. Zot와 일반 API의 ID namespace도 분리되지 않았다.

수정 방향: source/connection별 idempotency scope와 canonical payload hash를 저장한다. 동일 key/동일 payload는 기존 결과, 동일 key/다른 payload는 409를 반환한다. 클라이언트가 제공하는 ID와 서버 job identity의 구분도 필요하다.

### P2 — 이미지 미발견도 succeeded

실제 서버에 저장소에 없는 image 업데이트를 요청했다. remote HEAD는 그대로인데 job은 `succeeded`였다. 이미 적용돼 있는 값과 적용 대상을 찾지 못한 경우를 구분할 수 없다.

수정 방향: `already_satisfied`, `no_match`, `published`, `skipped_policy` 같은 outcome을 job 처리 완료 여부와 분리한다. `/api/status`의 success 집계도 이 의미를 반영해야 한다.

### P2 — GitHub signature는 확인하지만 repository scope는 확인하지 않음

유효한 HMAC 및 `refs/heads/main`과 함께 `repository.id=999`, `full_name=unrelated/repository`를 보내도 sync job이 저장됐다. 이는 signature 없는 요청이 통과했다는 뜻은 아니다. **같은 webhook secret으로 서명 가능한 다른 repository 이벤트를 구별하지 못한다**는 문제다.

현재 동작은 서버에 이미 설정된 저장소를 sync하는 데 한정되며 payload URL의 임의 저장소를 수정하지 않는다. multi-repo로 확장하기 전에 registered provider instance/native repository ID와 delivery scope를 검증해야 한다.

### P2 — retry endpoint가 DB 장애를 409로 응답

닫힌 DB에 대해 `POST /api/jobs/{id}/retry`를 실행하면 409와 `get job: sql: database is closed`가 반환됐다. 상태 충돌과 인프라 장애를 구분하지 못하며 내부 SQL 오류 문자열을 노출한다. 다른 route의 DB 장애는 500/503이었다.

수정 방향: typed storage error와 state-conflict error를 분리하고, 장애는 503/500과 안정된 error code로 응답한다. 상세 원인은 서버 로그로 제한한다.

### P2 — 명백히 잘못된 파일 경로도 admission 단계에서는 accepted

`file=../outside.yaml` 요청은 202로 저장됐다. 기존 `resolveWorkspaceFile()`은 worker에서 경로 탈출을 거절하므로 이를 외부 파일 쓰기 취약점으로 해석해서는 안 된다. 하지만 처음부터 실패할 요청이 accepted 뒤 일반 Git 실패와 같은 재시도 경로로 간다.

수정 방향: lexical path·허용 확장자는 admission에서 검증하고, snapshot의 존재 여부 및 symlink 검사는 worker에서도 유지한다. 재시도 가능한 일시 장애와 영구적인 invalid request를 구분한다.

## 4. 재실행

프로젝트 root의 PowerShell:

```powershell
$env:GOCACHE = Join-Path $env:TEMP 'git-updater-api-go-cache'
go test -buildvcs=false ./... -count=1
go vet ./...

$env:RUN_API_E2E = '1'
try {
    go test -buildvcs=false ./cmd/server -run TestAPIProcessLocalGit -count=1 -v -timeout 120s
} finally {
    Remove-Item Env:RUN_API_E2E -ErrorAction SilentlyContinue
}
```

이미 별도로 설정한 `RUN_API_E2E` 값이 있는 환경에서는 테스트 후 해당 값을 복원한다. E2E는 테스트 임시 경로의 Git 저장소만 사용하며 실제 registry.test 서버에 연결하지 않는다. registry.test는 manifest 안에 쓰는 fixture 이미지 이름이다.

결함 관찰에는 임시 Go overlay를 사용했다. 잘못된 동작을 영구적인 정답으로 고정하는 테스트는 저장소에 추가하지 않았다. 수정 단계에서는 위 요청들을 올바른 기대값(400/409/typed outcome 등)을 가진 회귀 테스트로 전환한다.

## 5. 변경과 남은 검증 범위

- 이번 커밋은 API 테스트 코드와 별도 점검 문서로 분리한다. production handler/worker 동작은 변경하지 않는다.
- 실제 Git transport 권한·provider identity·branch protection·registry artifact·원격 CAS 경쟁은 이번 API 테스트의 증거 범위 밖이다.
- 프로세스가 bind하는 주소는 기존 server의 `:PORT` 설정을 따른다. test client는 loopback을 사용하지만 서버가 loopback 전용으로 bind하는 것은 아니다.
- 본 테스트는 단일 프로세스·로컬 Git 검증이다. 운영 API의 외부 도메인 가용성, TLS, rate limit, 대규모 부하 또는 multi-Pod 안전성을 보장하지 않는다.
