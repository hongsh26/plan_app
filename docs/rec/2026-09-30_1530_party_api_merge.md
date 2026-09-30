# Party P2(생성·조회·수정 API) main 병합

## 완료

- `feature/party-api`를 `main`에 fast-forward 병합했다(`85d57b7..9796b2f`).
- 원격 CI(`server-ci`, `test` job)가 통과한 커밋을 병합했다. 마이그레이션 down 왕복, 파괴적 마이그레이션 가드, 로컬 밖 api 기동 거부 확인 포함.
- 서브 에이전트 독립 재검증에서 코드·테스트 문제는 없었고, 문서 불일치 2건은 병합 전에 반영했다.

## 범위

`POST /v1/parties`, `GET /v1/parties/{id}`, `PATCH /v1/parties/{id}`, `GET /v1/parties/{id}/memberships`. 동시 생성 한도 테스트에서 1건 성공·1건 409, `mutation.Retries()` 증가 0.

## 다음

P3 초대. 계정 삭제가 Party를 남기는 공백은 P8까지 살아 있다.
