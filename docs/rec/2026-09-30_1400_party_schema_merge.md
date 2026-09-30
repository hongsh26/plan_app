# Party 스키마 P1 병합

- 일시: 2026-09-30 14:00 KST
- 범위: `server/migrations/00010_party_membership.sql`, `server/test/party_schema_test.go`, 설계·진행 문서
- 결과: [PR #5](https://github.com/hongsh26/plan_app/pull/5)를 rebase 방식으로 `main`에 병합했다. 병합 커밋은 `85d57b76b804ab76ab842fb835b1b71cd065bea8`이다.

## 검증

- 최신 기능 브랜치 커밋 `64a5695`의 push CI와 PR CI에서 필수 `test`가 모두 통과했다. PostgreSQL 통합 테스트, `go vet`, `gofmt`, 마이그레이션 down/up 왕복이 포함됐다.
- 독립 재검증에서 migration 00010의 지연 트리거가 멤버십 `party_id` UPDATE의 이전·새 Party를 모두 검사하는지 확인했다. 회귀 테스트 `TestMovingMembershipRevalidatesPreviousParty`와 전체 서버 테스트가 통과했다.
- 재검증 중 전체 테스트에서 `TestIncrementalPayloadIsFullProjection`이 한 번 실패했으나 단독 재실행과 전체 재실행은 통과했다. 원인 미확인으로 남긴다.
- `docs/progressing.md`는 100줄 제한 안이다.

## 후속

- P2 Party 생성·조회·수정 endpoint. `calendar_busy_facts` 테이블이 아직 없어 생성 시 일정 projection 원본은 없다. 캘린더 동기화 구현에서 T1 projection 채우기를 연결한다.
- P8 계정 삭제와 Party 정리 공백은 여전히 남는다.
