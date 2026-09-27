# 웹 접근 및 HTTP 동작 점검

점검일: 2026-09-27. 대상 코드: `9ee7a62`.

## 결과

현재 제품에는 브라우저용 웹 UI가 없다. 실제 서버의 `GET /`는 `404 Cannot GET /`를 반환한다. Repository topology, image graph, 변경 preview, 작업 목록 화면은 테스트할 구현이 없으며, 웹 UI 검증 통과로 볼 수 없다.

브라우저 자동화는 Codex 내장 브라우저와 Chrome에서 로컬 서버 접속을 시도했으나 도구가 `net::ERR_BLOCKED_BY_CLIENT`를 반환했다. 내장 브라우저의 `127.0.0.1` 및 `localhost`, Chrome의 `127.0.0.1` 접속이 실패했다. 이는 브라우저 도구의 접속 차단 기록이며 제품 서버의 브라우저 호환성 결함으로 확정하지 않는다. 화면 렌더링, 브라우저 fetch, 사용자 조작 흐름은 **미검증**이다.

## 직접 HTTP 점검

실제 서버 바이너리를 빌드하고 별도 임시 디렉터리의 SQLite 및 local bare Git 저장소로 실행했다. 요청은 PowerShell HTTP client에서 전송했다. 아래 결과는 브라우저 실행 결과가 아니다.

| 요청 | 실제 결과 | 의미 |
|---|---|---|
| `GET /` | 404, `text/plain`, `Cannot GET /` | 웹 진입 페이지 없음 |
| `GET /health` | 200, `{"status":"healthy"}` | 프로세스 응답 정상 |
| `GET /ready` | 200, `{"status":"ready"}` | 현재 readiness 조건 충족 |
| `GET /api/status`, 인증 없음 | 401, JSON 오류 | 인증 필요 |
| `GET /api/status`, 유효한 테스트 Bearer | 200, counts 및 outcomes | 인증 후 작업 집계 조회 정상 |
| `GET /api/jobs` | 404 | 작업 목록 API 없음. 현재는 `/api/jobs/:id` 개별 조회만 제공 |
| `OPTIONS /api/update`, cross-origin preflight 헤더 | 405, `Access-Control-Allow-Origin` 없음 | 별도 origin의 웹 프런트엔드 연결을 위한 CORS 미구현 |

Preflight 점검은 `Origin: http://localhost:5173`, `Access-Control-Request-Method: POST`, `Access-Control-Request-Headers: authorization,content-type`으로 수행했다. 브라우저에서의 CORS 차단을 직접 관찰한 것은 아니다. 같은 origin에서 UI와 API를 제공하면 이 cross-origin 설정은 필요하지 않다.

소스에서도 `cmd/server/server.go`와 `cmd/server/router.go`에 API/webhook/health 라우트만 있으며, 현재 추적 대상 코드에 프런트엔드 package manifest나 HTML/Svelte/Vue/TSX 화면이 없다.

## 실제 HTTP → Git 통합 테스트

기존 프로세스 E2E 테스트를 다시 실행해 통과했다.

```powershell
$env:GOCACHE = Join-Path $env:TEMP 'git-updater-review-go-cache'
$env:RUN_API_E2E = '1'
go test -buildvcs=false ./cmd/server -run TestAPIProcessLocalGit -count=1 -v -timeout 120s
```

`TestAPIProcessLocalGit` 통과, 테스트 실행 시간 22.23초. 실제 TCP HTTP와 SQLite를 사용해 API/CLI/Zot 요청의 commit/push, 중복·이미 반영된 변경, 잘못된 tag/path, 충돌 ID, 이미지 미발견, GitHub repository ID 검사 및 sync, 자동·수동 retry, 재시작 후 기록 보존을 검증했다. 테스트 상세는 [API 점검 기록](api-verification.md)을 참고한다.

모든 테스트 commit/push는 임시 local bare 저장소에서만 수행했다. 외부 Git provider, registry, 운영 도메인/TLS, 클러스터 배포는 검증하지 않았다. 점검에 사용한 서버는 종료했다.

## 웹 UI 구현 시 필요한 범위

현재 API만으로 가능한 것은 집계 조회, ID를 알고 있는 작업 조회·retry, 즉시 업데이트 요청이다. 웹 Visualizer를 만들려면 다음 기능이 추가되어야 한다.

- 작업 목록 및 pagination, repository 상태 조회 API.
- 공통 Repository Model에 기반한 topology/image dependency 조회.
- 현재 API의 즉시 enqueue와 구분되는 ChangeSet preview/diff 및 명시적 실행 경로.
- 웹 사용자 인증과 권한 모델. 서버의 공용 API key를 프런트엔드 번들에 포함하지 않는다.
- UI를 같은 origin에 제공하거나, 별도 origin을 사용할 경우 허용 origin을 제한한 CORS 처리.

이 항목들은 이번 점검에서 새로 구현한 기능이 아니다. 전체 구조와 단계는 [controller 설계 검토](change-controller-design-review.md)를 따른다.
