-- 끝난 job 정리 경로. scheduler의 outbox_prune(jobs.PruneSucceeded)은
-- status = 'succeeded' AND updated_at < 보존 경계를 batch마다 찾는다.
-- outbox_jobs_claim_idx는 끝나지 않은 job만 담는 partial index라 이 조건을 받지 못하고,
-- index가 없으면 batch마다 전체 표를 훑는다.
--
-- CONCURRENTLY를 쓰지 않는다. 아직 운영 데이터가 없어 표가 비어 있다. 운영 데이터가
-- 생긴 뒤 큰 표에 추가하는 index는 goose의 NO TRANSACTION 지시어와 CONCURRENTLY로
-- 만든다. (주석 줄에 goose 지시어 표기를 그대로 쓰면 지시어로 파싱되어 실패한다.)

-- +goose Up
CREATE INDEX outbox_jobs_succeeded_updated_at_idx
    ON outbox_jobs (updated_at)
    WHERE status = 'succeeded';

-- +goose Down
DROP INDEX IF EXISTS outbox_jobs_succeeded_updated_at_idx;
