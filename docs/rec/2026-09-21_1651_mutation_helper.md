# mutation helper와 계정 mutation

- 날짜: 2026-09-21 16:51
- 브랜치: `feature/mutation-helper`
- 커밋: `1818528`(helper와 계정 mutation), `1f290cf`(리뷰 지적 반영)
- 성격: 기능 구현 완료, 버그 수정 완료, 설계·구현 방향 확정
- 기준: `docs/account_backend_design.md` §6, §7.1, §7.4, §8 / 구현 계획 5단계

## 범위

| 포함 | 미룸 |
|---|---|
| `internal/platform/mutation`: 멱등성, version 충돌, sync 변경 피드(ordinal 발급), outbox | `DELETE /v1/me`(계정 삭제 요청) |
| `PATCH /v1/me`(표시 이름) | sync 읽기 경로(`GET /v1/sync`, bootstrap, settled horizon) |
| `DELETE /v1/devices/{id}` | `idempotency_keys` 만료 row 정리(scheduler) |
| `PUT /v1/devices/{id}/push-token` | |
| 00002: push token과 environment 짝 제약 | |

**`DELETE /v1/me`를 미룬 이유.** §10의 1단계(`deletion_requested` 전환, 세션·기기 폐기)만
구현하면, 그 상태를 앞으로 진행시킬 worker가 없다. §5.3은 유예 기간 없이 즉시 차단하라고 하므로
요청한 계정은 되돌릴 방법 없이 영구히 잠긴다. 판단 기준은 "이 슬라이스가 아무것도 진행시킬 수
없는 도달 가능한 상태를 남기는가"다. 삭제 worker와 함께 넣는다.

## helper 계약 (확정)

- **키를 먼저 점유한다.** 트랜잭션 맨 앞에서 `idempotency_keys`를 `INSERT ... ON CONFLICT DO
  NOTHING`한다. 같은 키의 동시 요청은 unique index에서 앞 트랜잭션을 기다린다. 앞이 커밋했으면
  충돌(재전송 판정), 롤백했으면 이쪽 INSERT가 성공한다. 8개 동시 요청에서 실행은 정확히 1번이다.
- **실패는 기록하지 않는다.** 도메인 함수가 실패하면 idempotency row도 롤백된다. 같은 키로
  재시도하면 다시 실행된다. version 충돌처럼 결정적인 실패는 다시 실행해도 같은 결과다.
- **요청 지문 = method + 실제 경로 + 원본 본문 바이트.** JSON 정규화는 하지 않는다(규칙 자체가
  계약이 된다). **route 패턴이 아니라 실제 경로다.** 리뷰가 잡은 버그 참고. If-Match는 넣지 않는다.
- **재전송 응답은 저장된 포인터로 현재 리소스를 다시 읽어 만든다.** §8은 본문을 저장하지 말고
  포인터만 저장하라고 한다. 그래서 재전송 응답의 version은 원래 응답보다 클 수 있다.
  `Idempotent-Replayed: true` 헤더로 알린다. 재전송도 인증 미들웨어를 먼저 거친다(§8).
- **ordinal은 트랜잭션 단위 counter다.** PK가 `(txid, ordinal)`이므로 수신자별로 세면 PK 위반이다.
- **만료(30일)된 기록은 없는 것과 같다.** 지우고 다시 점유한다.
- **If-Match만 받는다.** §6은 If-Match 또는 본문 `expected_version`을 허용하지만, 둘 다 받으면
  둘이 다를 때의 규칙이 또 필요하다. 강한 ETag `"3"`과 따옴표 없는 `3`만 받는다. 모든 조회·변경
  응답의 `ETag` 헤더를 그대로 쓰면 된다.

## 잠금 규칙 (확정, 리뷰로 바뀜)

처음 문서에는 `idempotency_keys → users → devices → sessions`라고 적었다. **틀렸다.**
`idempotency_keys` INSERT는 FK 검사 때문에 `users` row와 요청 기기의 `devices` row에 **FOR KEY
SHARE**를 먼저 건다. 그 뒤 도메인 코드가 같은 row를 `FOR UPDATE`로 올리면, KEY SHARE를 쥔 두
트랜잭션이 서로의 업그레이드를 기다리며 교착한다.

- 재현: 한 사용자의 두 기기가 서로 다른 키로 동시에 `PATCH /v1/me`하면 거의 매번 한쪽이
  1초(deadlock_timeout) 뒤 500이었다. 한 기기가 서로 다른 키로 push token을 동시에 두 번 보내면
  30회 중 29회 500이었다(리뷰어 실측).
- **규칙: `users`·`devices` row는 `FOR NO KEY UPDATE`로 잠근다.** 두 표의 키 열은 바뀌지 않으므로
  충분하고, NO KEY UPDATE는 KEY SHARE와 충돌하지 않는다. `auth.LockUserAndDevice`도 바꿨다.
- **교착·직렬화 실패(40P01, 40001)는 트랜잭션 전체를 최대 3회 다시 실행한다.** 롤백된
  트랜잭션은 아무것도 남기지 않으므로 안전하다.
- 재시도는 규칙 위반을 숨긴다. 교착이 나도 다음 시도에서 성공하면 응답은 정상이고 1초 늦을
  뿐이다. 그래서 **`mutation.Retries()`로 누적 재시도 횟수를 노출하고, 회귀 테스트는 이 값이 0에서
  늘지 않았는지 본다.** 운영에서는 0이 아니면 잠금 규칙이 깨졌다는 신호다.

## 리뷰가 잡은 다른 High: 경로가 빠진 요청 지문

`DELETE /v1/devices/{id}`는 대상이 경로에만 있고 본문이 비어 있다. 지문에 route 패턴
(`DELETE /v1/devices/{id}`)만 넣었기 때문에, 같은 키로 기기 B를 지운 뒤 기기 C를 지우면 지문이 같아
**재전송으로 처리됐다.** 응답은 204였지만 C는 여전히 인증됐다. 사용자는 탈취된 기기를 폐기했다고
믿게 된다. 실제 경로(`r.URL.EscapedPath()`)를 넣어 고쳤다.

**두 버그 모두 제 테스트가 잡지 못했다.** 동시성 테스트는 같은 키만 썼다. 같은 키는 unique
index에서 직렬화되므로 교착이 날 수 없다. 키 재사용 테스트는 같은 대상만 썼다. 이번에 추가한
테스트는 고친 코드를 되돌리면 실패하는 것을 확인했다.

- `FOR NO KEY UPDATE` → `FOR UPDATE`로 되돌림: `TestConcurrentMutationsDoNotDeadlock` 실패
- 지문을 route 패턴으로 되돌림: `TestDeleteDeviceKeyReuseAcrossTargetsIsMismatch` 실패
  (`status = 204, want 409`)

## 그 밖의 결정

- **기기 폐기는 sync에 tombstone이 아니라 `revoked: true`를 담은 upsert로 보낸다.** row는 남고
  version이 오른다. 스키마 CHECK상 tombstone은 version을 가질 수 없다.
- **`DELETE /v1/devices/{id}`는 같은 사용자의 어느 기기든 지정할 수 있다**(§5.2). 남의 기기와
  이미 폐기된 기기는 404다. **`PUT .../push-token`의 `{id}`는 요청 기기 자신이어야 한다**(§6).
  아니면 존재 여부와 무관하게 403이다.
- **push token은 `secretbox`로 봉인해 저장하고 응답·sync·로그 어디에도 내보내지 않는다.** sync에는
  `has_push_token`만 싣는다. 그래서 `TOKEN_ENCRYPTION_KEY`가 Apple 자격 유무와 무관하게 항상 필수가
  됐다.
- **00002 마이그레이션:** `(push_token_ciphertext IS NULL) = (push_environment IS NULL)`. 00001의
  주석은 이 관계를 적었지만 제약이 강제하지 않았다. 적용된 00001을 고치지 않고 새 파일로 뒀다.
- 표시 이름 정규화(`auth.NormalizeDisplayName`)를 가입과 수정이 공유한다. 수정에서는 정규화 뒤
  비면 400이다(가입은 기본 이름으로 대신한다).
- audit 기록을 `internal/platform/audit`로 분리했다.

## 검증

- 로컬 `go test -race ./...`, `REQUIRE_DB_TESTS=1`: 전부 통과, skip 0
- 원격 CI(리뷰 전 `1818528`, run `35574529111`): PASS 195, SKIP 0. 00002가 CI에서 적용됐다
- 원자성: 도메인 쓰기, sync 변경, outbox job을 모두 쓴 뒤 실패를 주입해 넷(도메인, sync, outbox,
  idempotency) 모두 남지 않는 것을 확인한다
- 독립 리뷰(code-reviewer, opus): High 2건(위 두 건), Medium 2건(만료 키 동시 교체의 500, 오류 로그
  분류 누락), Low 3건. 모두 `1f290cf`에서 반영했다

## 남은 것

- `DELETE /v1/me`와 §10 삭제 파이프라인(worker)
- `idempotency_keys` 만료 row 정리(scheduler). 세션 정리와 함께
- sync 읽기 경로. `sync_changes`에 쓰기는 시작됐지만 읽는 곳이 아직 없다. settled horizon 읽기
  쿼리와 §15 순서 역전 회귀 테스트가 여기서 반드시 들어가야 한다
