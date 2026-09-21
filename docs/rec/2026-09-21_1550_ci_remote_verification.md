# server-ci 원격 실행 확인 (M-8 종결)

- 날짜: 2026-09-21 15:50
- 브랜치: `main` (확인 대상 커밋 `ea6d095`), 임시 브랜치 `feature/ci-red-check` (삭제함)
- 성격: 검증 완료

## 왜 필요했나

M-8은 `REQUIRE_DB_TESTS=1`을 켜는 CI가 없다는 지적이었다. `a611103`에서 워크플로를
추가했지만 그때는 로컬에서 각 스텝을 똑같이 실행해 본 것이 전부였다. 초록불 하나로는
"테스트가 돌아서 통과"와 "skip돼서 통과"를 구분할 수 없다. 그래서 두 가지를 확인해야
M-8을 닫을 수 있었다.

1. 원격에서 통합 테스트가 실제로 돌았는가 (`--- PASS`이고 `--- SKIP`이 아닌가)
2. 제약이 깨지면 빨간불이 나는가

## 1. 초록불의 내용 확인

- 실행: run `35433547813` (`main` push, `ea6d095`, 2026-09-19 09:01Z), 결론 `success`
- 로그 집계: `--- PASS` 64, `--- SKIP` 0, `--- FAIL` 0
- `plantogether/server/test` 패키지의 스키마 불변식 테스트
  (`TestAuthIdentityProviderSubjectIsUnique`, `TestOutboxDedupeKeyBlocksOnlyActiveJobs`,
  `TestSyncChangesShareTxidWithinTransaction` 등)가 PASS로 찍혔다. 즉 DB에 실제로
  접속해서 돌았다.
- "파괴적 마이그레이션 가드" 스텝이 `APP_ENV=production에서 reset이 가드에 의해 거부됐다.`를
  출력했다. M-5 가드가 진입점까지 연결돼 있다는 뜻이다.

## 2. 빨간불 확인

일부러 테스트 코드를 망가뜨리는 대신, 이 CI가 막으려는 실제 회귀를 흉내 냈다.
마이그레이션에서 유니크 제약이 사라진 상황이다.

- 변경: `server/migrations/00001_init.sql` 86행의
  `CREATE UNIQUE INDEX auth_identities_provider_subject_key`를 `CREATE INDEX`로 바꿨다
  (파일 1개, 1줄)
- 실행: run `35570157663` (`feature/ci-red-check`, 커밋 `aa34be5`), 결론 `failure`
- 실패한 테스트는 예상한 두 개였다.
  - `TestAuthIdentityProviderSubjectIsUnique`: `schema_invariants_test.go:174`,
    "제약 auth_identities_provider_subject_key가 위반을 거부하지 않고 삽입을 허용했다"
  - `TestDeletedAccountReleasesAppleSubjectForResignup`: `schema_invariants_test.go:518`,
    같은 메시지
- 실패한 스텝은 "테스트"였다. 연결 실패나 설정 오류가 아니라 제약 검증 때문에 실패했다.

확인이 끝난 뒤 원격과 로컬의 `feature/ci-red-check`를 모두 삭제했다. 이 커밋은 `main`에
들어가지 않았다.

## 결론

M-8은 닫혔다. 원격 CI가 통합 테스트를 실제로 돌리고, 스키마 제약이 회귀하면 빨간불이 난다.

## 하지 않은 것

- **`server-ci`를 required status check로 지정하지 않았다.** 저장소 설정을 바꾸는
  일이라 사용자가 결정해야 한다. 지정한다면 `paths` 필터 때문에 `docs/`만 바꾼 PR이
  pending 상태로 계속 남는 문제를 함께 처리해야 한다. 필터를 없애거나, 같은 이름의
  skip job을 둔다.
- 이 저장소의 GitHub 요금제에서 private 저장소에 branch protection을 쓸 수 있는지는
  확인하지 않았다.
