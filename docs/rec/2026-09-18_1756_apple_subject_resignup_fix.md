# 삭제된 계정의 Apple subject 해제와 재가입 경로 확정 (리뷰 M-3)

- 일시: 2026-09-18 17:56
- 종류: 버그 수정 완료 + 설계 방향 확정 (설계 2 개정)
- 대상: `docs/account_backend_design.md`, `docs/feature_design_backlog.md`, `server/migrations/00001_init.sql`, `server/test/schema_invariants_test.go`

## 계기

P0 서버 골격에 대한 독립 코드 리뷰에서 Medium으로 지적됐다. Critical/High는 없었다.

## 문제

§8 필수 불변식의 원문은 "외부 identity 하나는 **활성** 사용자 하나에만 연결된다"인데, 마이그레이션 주석이 이를 옮겨 적으면서 `활성`을 떨어뜨렸고 index도 조건 없는 unique였다.

삭제된 사용자의 `users` row는 §5.3에 따라 tombstone으로 남는다. `auth_identities` row도 함께 남으면 `(provider, provider_subject)` unique가 같은 Apple 계정의 재가입을 **영구히** 막는다. 이는 §10 7단계의 "재로그인을 새 계정 생성 흐름으로 처리한다"와 정면으로 충돌한다.

리뷰어가 실제 PostgreSQL에서 재현했고, 개정 후 같은 시퀀스를 다시 실행해 거부를 확인했다:

```
INSERT users(old) + auth_identities(apple, S)
UPDATE users SET status='deleted'        -- 물리 삭제를 빼먹은 경우
INSERT users(new) + auth_identities(apple, S)
-- ERROR: duplicate key ... "auth_identities_provider_subject_key"
```

## 확정

**§10 7단계에서 `deleted` 전환과 `auth_identities` row 물리 삭제를 같은 트랜잭션에서 커밋한다.**

### 왜 3단계가 아니라 7단계인가

- 3단계는 Apple revoke 호출에 refresh token ciphertext가 필요하다.
- 3→7단계는 재시도를 포함해 최대 24시간에 걸칠 수 있다(§10의 자체 목표).
- 3단계에서 지우면 그 사이에 같은 Apple 계정으로 로그인한 사용자가 새 계정을 만들고, 삭제 진행 중인 기존 `users` row와 함께 한 subject에 두 계정이 생긴다. 어떤 제약도 이를 막지 못한다.
- 7단계로 두면 경계가 §5.3의 상태 정의와 정확히 일치한다. `deletion_requested`·`deleting` 동안에는 row가 있어 로그인이 차단된 기존 계정으로 해석되고, `deleted` 이후에는 row가 없어 새 계정 생성이 된다.

### 탈락시킨 대안: `detached_at` + partial unique

`auth_identities`에 `detached_at` 같은 열을 두고 `WHERE detached_at IS NULL` 조건부 unique로 가는 방식. 삭제된 계정에 대해 Apple의 **안정적 subject를 계속 보존**하게 된다. subject는 개인 식별자이므로 §4 데이터 최소화 및 §10의 삭제 기대와 충돌한다. 설계 문서와 마이그레이션 주석 양쪽에 "partial unique로 바꾸지 말 것"을 근거와 함께 남겼다.

## 남긴 미결 질문

`disabled → deleted`가 §5.3 상태 전이도에서 허용되므로, 운영상 차단된 사용자가 계정을 삭제하고 재가입하면 차단이 풀린다. 물리 삭제가 이를 가능하게 한다.

막으려면 삭제 후에도 subject 해시를 보존해 대조해야 하는데, 방금 닫은 개인정보 트레이드오프를 다시 연다. 실제 남용이 관측되기 전에는 결정하지 않기로 하고 `docs/feature_design_backlog.md`의 "아직 확정되지 않은 핵심 정책"에 트레이드오프와 함께 기록했다.

## 변경

- `docs/account_backend_design.md`: §1(개정 이력), §8(불변식에 `활성` 복원과 근거, partial unique 금지), §10 7단계(물리 삭제 명시와 단계 선택 근거), §14(재가입 인수 조건)
- `docs/feature_design_backlog.md`: 차단 회피 미결 질문 추가
- `server/migrations/00001_init.sql`: index 주석을 정정. index 자체는 바뀌지 않았고, **조건 없는 unique가 정확한 이유가 7단계의 물리 삭제에 의존한다**는 사실을 명시했다. 이 주석이 원래 `활성`을 떨어뜨린 지점이다.
- `server/test/schema_invariants_test.go`: `TestDeletedAccountReleasesAppleSubjectForResignup` 추가

## 검증

- `gofmt`, `go vet` 통과
- 실 PostgreSQL 18.2에서 신규 테스트 PASS. `deleting` 구간의 재가입 거부 → `deleted` + 물리 삭제 → 재가입 성공 + 새 `user_id` → 잔존 identity 0건까지 확인
- **테스트가 공허하지 않음을 실증했다.** 물리 삭제를 뺀 시퀀스를 psql로 직접 돌려 `duplicate key` 거부를 확인했다. 삭제 파이프라인에서 물리 삭제가 빠지면 이 테스트가 실패한다.

여기까지가 M-3 처리다. 같은 세션에서 반영한 나머지는 아래에 이어진다.

---

## 추가 반영 (같은 세션, 18:02)

M-3 외에 리뷰 지적 4건을 함께 반영했다.

| # | 변경 | 검증 |
|---|---|---|
| M-2 | `audit_events_actor_check`를 `(A IS NULL) <> (B IS NULL)`에서 `NOT (A IS NOT NULL AND B IS NOT NULL)`로. 둘 다 NULL(행위자 없음)을 허용한다 | 실 DB에서 `action='auth.apple.verify', result='failure'` 삽입 성공, 둘 다 있는 row는 여전히 거부 |
| M-4 | `outbox_jobs_claim_idx`의 `WHERE`에 `running` 추가 | `pg_indexes`로 `ARRAY['pending','retryable_failed','running']` 확인 |
| M-6 | `.gitignore`: `server/api`·`server/worker`·`server/scheduler` 추가, `*/.omc/`·`*/*/.omc/` 추가 | `git check-ignore -v`로 매칭 확인. 루트 `.omc/plans/` 추적은 유지됨을 함께 확인 |
| M-1, L-2 | 주석 근거 정정 | — |

**M-2의 근거**: §11이 별도 rate limit을 요구하는 인증·초대 코드 검증은 정의상 `user_id`가 없는 시점의 이벤트다. Apple credential 검증 실패와 rate limit 거부가 대표적이고 가장 감사하고 싶은 이벤트인데, 행위자를 필수로 만들면 스키마에 들어가지 못해 가짜 actor를 만들어 넣게 된다.

**M-4의 근거**: §9의 "worker crash 후 `locked_until`이 지나면 다른 worker가 안전하게 재개한다"가 성립하려면 `status='running'` + 만료 `locked_until` row를 집어야 한다. `running`이 빠지면 P7에서 index에 맞춰 짠 claim 쿼리가 고아 row를 영원히 못 집고, `dead`로도 못 가서 §14의 dead job 경보에도 안 걸린 채 조용히 멈춘다.

**M-1의 판정**: 리뷰어는 FK 생략 자체는 §8 위반이 아니라고 판정했다(§8의 "협업 기록"은 §10 5단계 대상이고 보안 audit는 §10 6단계의 별개 절차). 다만 원래 주석의 근거("FK가 있으면 치환이 제약 위반이 된다")가 사실이 아니었다. 별도 `actor_tombstone` 열을 둔 이상 치환은 `actor_user_id`를 NULL로 바꾸는 것이어서 FK가 있어도 위반이 아니다. 주석을 실제 근거로 교체하고, FK가 없으므로 치환을 DB가 강제하지 않는다는 사실을 명시했다.

**M-6에서 리뷰에 없던 것을 추가로 발견**: 루트 `.gitignore`의 `.omc/*`는 루트에만 걸려 `server/.omc/state/`가 커밋 대상에 올라와 있었다. 에이전트가 `server/`를 cwd로 실행하면서 생긴 것이다. `**/.omc/`로 쓰면 루트 `.omc/` 디렉터리 자체가 제외되어 `!.omc/plans/` 부정 패턴이 무력화되므로 `*/.omc/`와 `*/*/.omc/`로 한 단계씩 적었다.

## 남은 리뷰 지적

M-5, M-8, M-9와 L-1·L-3·L-4·L-5·L-6은 반영하지 않았다. 목록과 각각의 위험은 `docs/progressing.md`의 표에 있다. 가장 급한 것은 **M-5**(프로덕션 `api` 바이너리가 `-migrate reset`으로 스키마 전체를 지울 수 있다)와 **M-8**(CI가 없어 통합 테스트 8개가 전부 skip되고 `ok`로 보인다)이다.
