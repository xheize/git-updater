# 현재 구현 분석 및 검증 결과

검토일: 2026-09-27. 기준 코드: `a104778`. 기존 updater의 구조, 목표 대비 gap, 결함과 재현 결과를 기록한다. 실행 코드 수정은 포함하지 않는다.

## 1. 현재 구현과 gap

현재 흐름:

```text
CLI / API / Zot → JobStore(SQLite) → 단일 worker
  → fetch + hard reset → imageToFiles 조회
  → 파일별 YAML decode/encode → commit → push
GitHub push → sync Job → fetch/reset + imageToFiles rebuild
```

| 영역 | 현재 코드 | 목표와의 차이 / 재사용 방향 |
|---|---|---|
| 입력 | `cmd/server/router.go`, `cmd/cli/cli.go` | 인증·route·CLI 재사용. Job 직접 생성 대신 Intent adapter로 전환 |
| 큐 | `internal/gitManager/job_store.go` | SQLite 영속화·claim·backoff 재사용. payload fingerprint, outcome, plan, publish attempt 추가 |
| Git | `internal/gitManager/gitManager.go` | SSH known_hosts·HTTP 인증·timeout·author·local Git 테스트 재사용. snapshot과 publisher 분리 |
| 이미지 검색 | `imageToFiles`, `extractImagesFromNode` | 모든 scalar `image`를 찾는 파일 인덱스. resource/container/target/provenance가 없음 |
| YAML 수정 | `internal/yaml/updater.go` | multi-document node 순회는 재사용 가능. 전체 serializer를 최소 변경 writer로 교체 |
| Kustomize / Helm | 전용 해석기 없음 | build target, renderer, provenance resolver 추가 |
| 저장소 검증 | clone/fetch 실패로 일부 접근성 확인 | provider identity, credential capabilities, branch rules 검증 없음 |
| 정책 | 전역 `AUTO_UPDATE`, 요청의 `Force` | target별 정책·호출자 권한·승인 없음 |
| 작업 단위 | 단일 image/tag Job, 여러 파일을 한 commit | 여러 image를 묶은 ChangeSet, preview, immutable plan 없음 |
| 시각화 | status/jobs API | snapshot graph, impact, diff, semantic history 없음 |
| 운영 | 단일 Pod, Recreate, DB PVC | 현재 단일 writer 전제와 일치. replicas만 늘리는 확장은 불가 |

### 우선 수정할 현재 결함

**P1 — fetch 이후에도 이전 revision의 인덱스로 수정 대상을 결정한다.**

`Work()`는 `syncRepository()` 이후 `imageToFiles`를 재생성하지 않는다 (`gitManager.go:415`, `:429`). 시작 시 인덱스 생성 이후 remote에 새 manifest가 추가되면, 해당 파일은 fetch되지만 이번 업데이트 대상에서는 빠진다. 갱신 후에야 mapping을 다시 만든다. webhook sync job이 먼저 도착한다는 보장도 없다.

로컬 Git 재현: `old.yaml`로 인덱스를 만든 뒤 origin에 같은 image의 `added.yaml`을 추가했다. `Work()`는 성공했지만 `added.yaml`은 old tag로 남았다. snapshot과 index를 반드시 동일 revision으로 묶어야 한다.

**P1 — 기존 workspace와 설정 repository의 identity를 비교하지 않는다.**

`New()`는 고정 `./workspace`의 저장소를 열고 기존 `origin`을 sync한다 (`gitManager.go:76`, `:93`, `:106`). 설정 URL이 다른 repository로 바뀌어도 기존 origin이 정상 작동하면 그 저장소를 계속 사용한다. 이는 정적 코드 경로에서 확인한 위험이며, 이번 검토에서 실제 외부 저장소를 변경하지 않았다. canonical identity별 workspace 분리와 origin binding 검증이 필요하다.

**P1 — image 필드라는 이유만으로 의미를 추정한다.**

`extractImagesFromNode()`와 `UpdateImageInNode()`는 Kubernetes workload 경로를 제한하지 않는다. ConfigMap의 `data.image: nginx:old`도 변경됨을 재현했다. 반면 Kustomize의 `images[].name/newTag`는 인식하지 못했다. 디렉터리 내 문서가 모두 배포 입력이라는 보장도 없다.

**P1 — digest 문법과 tag 변경 의미가 불완전하다.**

`getBaseImageName()`은 마지막 colon으로 분리한다 (`gitManager.go:677`). `nginx@sha256:...`를 `nginx@sha256`으로 잘못 색인할 수 있다. updater는 digest-only 참조를 찾지 못하고, `nginx:old@sha256:...`에 tag 변경을 적용하면 digest를 제거한다. 후자는 helper 직접 호출로 재현했다. OCI/Docker reference parser로 tag와 digest를 별도 필드로 보존해야 한다.

**P1 — 늦은 재시도가 새 intent를 되돌릴 수 있다.**

실패한 v1 작업이 backoff 중일 때 v2 작업이 성공하고, 이후 v1 재시도가 실행되면 최신 HEAD에 v1을 쓸 수 있다. fetch가 최신이어도 intent가 최신이라는 뜻은 아니다. 현재 expected value, promotion sequence, supersession 규칙이 없다. 이 시나리오는 코드 기반 분석이며 이번 실행에서 별도 재현하지 않았다.

**P2 — 성공 상태가 서로 다른 결과를 합친다.**

`AUTO_UPDATE=false`, 이미지 미발견, 이미 목표 tag인 경우도 성공 처리한다. `published`, `already_satisfied`, `skipped_policy`, `no_match`, `unsupported`를 구분해야 UI가 실제 변경 여부를 설명할 수 있다. 형식 오류/권한 오류까지 같은 backoff로 재시도하는 것도 수정한다.

**P2 — 파싱 실패를 정상적인 검색 완료처럼 처리한다.**

`buildImageMapping()`은 읽기 실패를 건너뛰고 decoder 오류를 EOF와 동일하게 처리한다 (`gitManager.go:598–610`). 일부만 해석한 index로 전체 영향이 없다고 결론 내릴 수 있다. diagnostics와 completeness를 반환해야 한다.

**P2 — 최소 diff를 보장하지 않는다.**

`ProcessYAMLImageUpdate()`는 모든 문서를 `SetIndent(2)`로 다시 encode한다 (`updater.go:165–173`). 4칸 들여쓰기가 2칸으로 바뀌는 것을 재현했다. 기존 테스트의 하나는 parse된 의미만 비교하므로 포맷 보존을 증명하지 않는다.

**P2 — deduplication과 복구의 보장 범위가 좁다.**

`ON CONFLICT(id) DO NOTHING`은 같은 ID/다른 payload도 조용히 수락한다. Zot ID는 source scope가 없고, ID가 없는 재전송은 새 작업이 된다. GitHub는 signature와 ref만 확인하고 payload repository ID는 확인하지 않는다. 현재 효과는 지정 workspace sync이지만 multi-repo routing에 그대로 확장하면 안 된다.

`RecoverInterruptedJobs()`는 모든 running 작업을 pending으로 돌리므로 여러 active worker가 공유하는 복구 방식이 아니다. DB의 결과 기록과 remote push 사이에도 atomic transaction이 없다.

### 이미 있는 보호 장치

현재 push는 무조건 force push하지 않는다. 일반적인 non-fast-forward 경쟁은 Git에 의해 거절될 수 있다. 여러 파일 처리 중 오류가 나면 commit 단계에 진입하지 않으며 이를 검증하는 테스트도 있다. 따라서 현재 코드가 언제나 부분 commit을 만든다고 평하는 것은 부정확하다. 다만 실패 전 파일 변경은 workspace에 남고, 검증된 plan/격리된 작업 공간/명시적 publish 복구 기록은 없다.

## 2. 검증 결과

- 초기 working tree는 clean, 기준 commit은 `a104778`이었다.
- `go test -buildvcs=false ./...`: 전체 통과. Windows sandbox에서 기본 GOCACHE와 임시 Git clone 접근이 차단되어, writable cache와 sandbox 밖의 로컬 테스트 실행으로 확인했다.
- 임시 Go overlay를 사용해 저장소의 production/test source를 바꾸지 않고 YAML 한계 5가지와 fetch 이후 stale index를 재현했다.
- YAML 관찰: 들여쓰기 변경, digest-only 무변경, tag+digest의 digest 제거, Kustomize newTag 무변경, ConfigMap data.image 변경.
- local origin/clone 재현: remote에 추가된 image 사용처가 update에서 누락돼도 Work가 성공했다.
- 실제 provider credential, 외부 registry, branch protection, 원격 Git 서버 CAS 경쟁, Argo CD/cluster 동작은 실행 검증하지 않았다. 관련 설계는 source 검토와 공식 문서에 근거한다.
- 검토 및 재현 과정에서는 외부 저장소에 commit/push하거나 배포하지 않았다. 이 문서는 검토 결과를 기록한다.
