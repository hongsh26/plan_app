package syncfeed

// Epoch는 cursor를 발급한 PostgreSQL 클러스터의 세대다.
//
// 왜 필요한가: cursor는 (txid, ordinal) 위치이고, 다음 읽기는 그 뒤만 돌려준다.
// 비동기 복제본으로 장애 전환하거나 시점 복구(PITR)를 하면 마지막 커밋 일부가
// 사라지고 새 primary의 다음 txid가 클라이언트 cursor보다 작아질 수 있다. 그러면
// 이후 변경이 전부 cursor 앞에 쓰여 오류 없이 영원히 전달되지 않는다.
//
// SystemID(pg_control_system().system_identifier)는 initdb마다 다르다.
// Timeline(pg_control_checkpoint().timeline_id)은 승격과 시점 복구 때 늘어난다.
// 둘 중 하나라도 다르면 cursor는 410이고 클라이언트는 bootstrap을 다시 받는다.
//
// 남는 위험: 같은 timeline 안에서 파일시스템 스냅샷으로 되돌리면 둘 다 그대로다.
// 그런 복구를 하면 운영 절차로 모든 클라이언트에 재동기화를 강제해야 한다.
// timeline은 승격 직후 첫 checkpoint에서 바뀌므로 그 사이 짧은 창이 있다.
// PostgreSQL은 승격 때 즉시 checkpoint를 요청한다.
//
// sync 읽기는 primary에서 해야 한다. 복제본의 horizon과 timeline은 primary와 다를 수 있다.
type Epoch struct {
	SystemID uint64
	Timeline uint32
}

// epochSQL은 현재 세대를 읽는 SQL 조각이다. 읽기와 bootstrap이 같은 식을 쓴다.
const epochSQL = `(SELECT system_identifier FROM pg_control_system())::text,
       (SELECT timeline_id FROM pg_control_checkpoint())::text`
