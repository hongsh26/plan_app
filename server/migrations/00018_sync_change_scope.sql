-- Party 범위 sync 변경에 수신자의 당시 멤버십 ID를 싣는다(account_backend_design.md §7.1 2026-10-28 개정,
-- docs/rec/2026-10-28_1613_party_sync_propagation.md).
--
-- 피드는 txid 순이지만 txid는 Party 잠금 전(멱등성 INSERT)에 배정돼 직렬화 순서와 어긋날 수 있다. 그러면
-- 탈퇴자 scope 정리용 version 없는 tombstone 뒤에 먼저 직렬화된 트랜잭션의 upsert가 온다. 클라이언트는 이 값으로
-- 끝난 멤버십으로 발행된 upsert를 버리고, 다른 멤버십으로 받은 사본은 version 없는 tombstone으로 지우지 않는다.
--
-- 행은 이미 수신자별이라(recipient_user_id) 값도 수신자별로 정확하다. 파생 피드라 FK를 두지 않는다(멤버십 row는
-- 지우지 않지만 피드 정리 순서에 묶이지 않게 한다). Party 밖 entity는 NULL이다.

-- +goose Up
ALTER TABLE sync_changes ADD COLUMN scope_membership_id uuid;

-- +goose Down
ALTER TABLE sync_changes DROP COLUMN scope_membership_id;
