# 활성 Party bootstrap scope 추가

## 기능 구현

- 전체 동기화 snapshot에 호출 사용자가 속한 활성 Party, 활성 멤버십, 본인 공개 설정, 허용된 일정 projection을 포함했다.
- 활성 초대는 방장에게만 포함한다. 초대 token과 일정 상세 ciphertext는 snapshot에 넣지 않는다.
- 계정과 Party entity를 동일한 REPEATABLE READ 트랜잭션에서 읽어 watermark와 snapshot의 기준을 맞췄다.

## 검증

- `GOCACHE=/private/tmp/plan_app_go_cache go test ./...` 통과(로컬 포트가 필요한 테스트는 샌드박스 밖 재실행).
- `go vet ./...` 통과.
- 로컬 PostgreSQL schema version 15에서 `REQUIRE_DB_TESTS=1 go test -p 1 ./internal/party ./internal/syncfeed -count=1` 통과. 새 bootstrap scope 테스트를 포함한다.
- `REQUIRE_DB_TESTS=1 go test -p 1 ./... -count=1` 전체 통과. 첫 실행에서는 공유 로컬 DB에 남아 있던 오래된 snapshot 세션 때문에 calendar prune 테스트의 삭제 건수가 5로 나왔고, 단독 재실행과 전체 재실행에서 통과했다.

## 후속 작업

- iOS에서 cursor 만료 시 Party projection 저장소를 bootstrap snapshot으로 원자 교체하고, 로그아웃·멤버십 종료 때 캐시를 지운다.
- 탈퇴·강퇴·해산 전이의 sync tombstone과 계정 삭제 연동을 구현한다.
