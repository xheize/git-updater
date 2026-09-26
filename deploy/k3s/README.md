# k3s 운영 배포

이 예제는 SSH 인증, amd64 이미지, k3s `local-path` 스토리지를 기준으로 한다. `kubectl apply`는 실제 클러스터를 변경하므로 아래 준비와 검증을 마친 뒤 실행한다. 저장소에 실제 Secret을 커밋하지 않는다.

## 운영 조건

- **replicas=1, Recreate**: SQLite와 Git 워크스페이스는 단일 워커 전용이다. HPA/여러 Pod를 지원하지 않는다. Recreate는 업데이트 중 구·신 Pod 동시 실행을 막으며 짧은 중단이 발생한다. 수동 중복 실행도 피한다.
- `/app/data`는 PVC에 저장한다. SQLite의 DB, `-wal`, `-shm`은 같은 로컬 볼륨에 둔다. NFS/RWX 공유 볼륨을 사용하지 않는다. `local-path`는 노드 로컬 저장소이므로 노드 손실에 대비한 별도 백업이 필요하다.
- `/work`는 emptyDir이며 저장소는 그 아래 `workspace`에 다시 clone한다. `/app/workspace`나 `/work/workspace` 자체에 볼륨을 마운트하지 않는다. 초기화에서 해당 디렉터리를 제거할 수 있기 때문이다.
- UID 100/GID 101을 이미지와 manifest에 일치시켰다. PVC provisioner가 `fsGroup: 101`을 반영해 쓰기 권한을 제공하는지 확인한다. Pod가 `permission denied`로 시작하지 못하면 해당 PV의 소유권을 조정한다.
- 기본 CI는 다중 아키텍처 이미지를 게시하지 않는다. amd64 노드에 배치하며 ARM 사용 시 별도 ARM 이미지 빌드·검증이 필요하다.

## 준비

1. 배포 YAML의 `REPLACE_WITH_REVIEWED_TAG`를 **이 PR이 포함된 빌드의 고정 태그 또는 digest**로 바꾼다. `v0.0.3`과 이전 이미지에는 새 설정·상태 API가 없다. 기존 배포의 selector와 Service 이름, namespace도 비교한다.
2. namespace를 만들고 Secret을 준비한다. 아래 `server.env`는 저장소 밖에 권한을 제한해 보관한다. `API_KEY`, `GIT_REPOSITORY_URL`을 포함해야 한다. GitHub 웹훅을 사용할 때는 `GITHUB_WEBHOOK_SECRET`도 추가하고 Deployment의 `GITHUB_WEBHOOK_ENABLED`를 `true`로 바꾼다. 설정하지 않으면 `/webhook/github`는 404다.

```bash
kubectl apply -f deploy/k3s/namespace.yaml
kubectl -n gitupdate create secret generic git-updater-server-env \
  --from-env-file=/secure/server.env --dry-run=client -o yaml | kubectl apply -f -
kubectl -n gitupdate create secret generic git-updater-ssh \
  --from-file=ssh-privatekey=/secure/git-private-key \
  --from-file=known_hosts=/secure/verified-known-hosts \
  --dry-run=client -o yaml | kubectl apply -f -
```

`known_hosts`는 Git 제공자의 공식 지문과 대조한 키를 사용한다. `ssh-keyscan` 출력만으로 신뢰하지 않는다. 기존 `SSH_KNOWN_HOSTS` 변수는 새 `GIT_SSH_KNOWN_HOSTS_FILE` 설정을 대체하지 않는다. Git 작성자는 `GIT_AUTHOR_NAME`/`GIT_AUTHOR_EMAIL`로 설정하며, 기본값은 `git-updater`/`git-updater@localhost`다. 저장소 접근 키에는 대상 저장소 push 권한이 필요하다.

사설 레지스트리는 기존 노드 인증을 사용하거나 별도 imagePullSecret을 생성하고 Pod의 `imagePullSecrets`에 연결한다. 예제는 Secret 값을 포함하지 않는다. 외부 웹훅을 받을 때는 별도의 TLS Ingress/Gateway를 연결하고 `/api` 접근 범위를 제한한다.

## v0.0.3 또는 PVC 없는 배포에서 전환

1. webhook/CLI 발신을 잠시 중단하고 기존 작업을 완료한다. 이전 메모리 큐는 새 SQLite 큐로 자동 이전되지 않는다. 미완료 요청은 전환 후 다시 전송한다.
2. SQLite 사용 버전인데 PVC가 없었다면 **기존 Pod를 삭제하기 전에** 작업을 멈춘 상태에서 SQLite backup API 등으로 일관된 백업을 만든다. 실행 중인 `jobs.db` 파일만 복사하면 WAL에 남은 작업을 잃을 수 있다. 새 PVC로 백업을 복원한 뒤 시작한다.
3. 기존 manifest를 보관한다. Secret, PVC를 먼저 준비하고 다음 출력에서 이미지·볼륨·selector·strategy를 검토한다. 기존 `rollingUpdate` 필드가 남는지 특히 확인한다.

```bash
kubectl kustomize deploy/k3s
kubectl apply --dry-run=server -k deploy/k3s
kubectl diff -k deploy/k3s
kubectl apply -k deploy/k3s
kubectl -n gitupdate rollout status deployment/git-updater --timeout=240s
```

GitOps로 관리한다면 위 변경을 해당 GitOps 저장소에 반영한다. 수동 apply와 GitOps 컨트롤러가 서로 덮어쓰지 않게 한다. PVC는 Deployment 롤백과 별도로 보존한다. 메모리 큐 버전으로 롤백하면 SQLite에 남은 작업을 처리하지 못하므로 DB와 미완료 작업을 먼저 보존한다.

## 배포 후 검증

```bash
kubectl -n gitupdate get pods,pvc
kubectl -n gitupdate logs deployment/git-updater --tail=100
kubectl -n gitupdate port-forward service/git-updater-service 3000:80
# 다른 터미널에서 실행
curl -f http://127.0.0.1:3000/health
curl -f http://127.0.0.1:3000/ready
curl -f -H "Authorization: Bearer $API_KEY" http://127.0.0.1:3000/api/status
```

- `/health`: HTTP 프로세스 생존 확인. Git push 성공을 뜻하지 않는다.
- `/ready`: 종료 중이 아니고 워커가 종료되지 않았으며 DB에 연결할 수 있는지 확인한다. Git 권한·원격 장애·DB 쓰기 권한을 모두 검증하는 probe는 아니다.
- `/api/status`: 인증된 작업 상태별 수와 현재 성공/실패·재시도 레코드의 최근 갱신 시각. `failed`/`retrying` 증가에 알림을 설정하고 각 job ID의 `/api/jobs/{id}` 및 로그로 조사한다. 과거에 실패했지만 이후 성공한 작업은 실패 집계에서 빠진다. `/ready`를 실패한 작업 때문에 차단하지 않아 수동 재시도 API를 계속 사용할 수 있다.
- 테스트 저장소에서 CLI로 실제 태그 변경을 요청하고 job이 `succeeded`인지, 원격 커밋의 작성자와 이미지 값이 맞는지 확인한다. 같은 태그 재요청도 성공해야 한다.
- 테스트 환경에서 Pod 교체 후 같은 job ID의 상태가 남고, 미완료 작업이 복구되는지 확인한다. Secret 변경 후에도 Pod를 교체해 새 설정을 읽게 한다.

SIGTERM을 받으면 새 작업 claim을 중단하고 현재 작업을 마친 뒤 종료한다. Git clone/fetch/push는 각각 30초 제한이며 종료 유예는 120초다. 강제 종료된 running 작업은 다음 시작에 복구된다. 프로세스 하나 안의 제한이므로 공유 PVC를 여러 워커가 동시에 사용하는 구성을 허용하지 않는다.

참고: [Deployment 전략](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/), [볼륨 권한](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/), [K3s 스토리지](https://docs.k3s.io/storage).
