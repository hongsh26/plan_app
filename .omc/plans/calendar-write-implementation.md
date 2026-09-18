# 확정 일정 외부 캘린더 쓰기 구현 계획

기준 설계: `docs/calendar_write_design.md`

## 전제

- 사용자 결정에 따라 상세 설계 9까지 완료한 뒤 구현을 시작한다.
- 서버 골격(P0), Party membership, calendar connection·snapshot, proposal 확정(Q3)이 선행한다.
- W1은 `confirmed_events`에 컬럼을 추가하므로 Q3의 `confirmed_events` 스키마가 이미 적용되어 있어야 한다. 두 마이그레이션이 같은 릴리스에 들어가면 Q3 → W1 순서를 강제한다.
- 기존 Swift 데모는 각 단계에서 계속 빌드·실행 가능해야 한다.
- 구현 브랜치는 `feature/calendar-write`를 사용한다.

## 단계

### W0. 상태 전이 계약 테스트

- confirmed event `active → cancelled` 전이표와 종단 상태 역전이 금지
- revision 발급 규칙표의 전 조합
- 이력 기반 delete 발급 판정(`ever_claimed`·`succeeded` 세대 유무), `/rewrite` 후 취소와 lease 만료 후 취소 경로
- backoff·최대 시도 종료, lease 만료 회수의 `attempt_count` 증가, 종일·time zone 재구성 fixture
- 완료 기준: 설계 §13 「순수 단위 테스트」 목록(revision 발급 규칙표, 이력 기반 delete 판정, backoff, 상태 전이표, 종일·time zone 재구성)이 테스트로 표현되고 구현 전 실패한다. DB 제약·iOS·E2E 조건은 각각 W1·W6·W8이 담당한다

### W1. 스키마와 제약

- `confirmed_events` 취소 필드, `confirmed_event_participants` reservation status
- `calendar_write_commands` 세대 unique, 활성 command partial unique, `ever_claimed` flag
- `calendar_connections`의 `source_status`/`destination_status` 축 분리와 설계 3 gate 질의 갱신
- 본인 전용 `calendar_cleanup_suggestions` 테이블
- `app_confirmed_event_id`는 nullable 추가만 하고 backfill·강제 re-snapshot 없음(구세대는 reservation 제외로 커버)
- `proposals.supersedes_confirmed_event_id`(불변)와 열린 대체 제안 partial unique
- reservation partial exclusion constraint 전환과 취소 후 재확정 가능성
- sync entity type과 domain outbox type 확장. 설계 9가 추가한 `calendar_cleanup_suggested`(dedupe `confirmed_event_id:user_id`)를 포함한다
- 완료 기준: 중복 활성 command, 중복 세대, 겹치는 reserved 구간이 DB에서 거부된다

### W2. 확정 시 command 발급

- 설계 7 T3 확정 transaction에 참여자별 create command 삽입
- writer device 미등록 사용자의 지연 할당
- 본인 전용 `calendar_write_status` sync와 `confirmed_event_created` outbox
- 완료 기준: confirm 롤백 시 command가 남지 않고, 활성 confirmed event마다 `참여자 수 = 활성 command 수 + 종단 완료 사용자 수`가 항상 성립한다

### W3. Claim·Lease·결과 보고 API

- writer device 검증, 본인 스코프 질의, `FOR UPDATE SKIP LOCKED` claim, 90초 lease
- `executor_device_id` NULL의 claim 시 지연 할당
- 결과 보고 멱등성, `stale_lease`·`command_cancelled` 거절
- backoff scheduler, lease 만료 회수(`attempt_count` 증가, 상한 초과 시 `exhausted`), `end_at + 6시간` 경과 create의 `expired_window` 종료
- writer device 전환 endpoint와 미완료 command 재할당·lease 무효화 transaction
- `calendar_write_command_pending` scheduler(확정 후 2시간, 24시간 간격 최대 3회)
- 완료 기준: 동시 claim에서 단일 획득, 타 사용자 command 미노출, 만료 lease 보고 거절, 8회 후 `exhausted` 종료

### W4. 취소·재일정

- 권한(현재 활성 멤버 전제 + 원 제안자·현재 방장), expected version, 종단 상태 재취소 거절
- `claimed` create 취소 시 delete `next_run_at` 지연
- destination 재선택의 `user_action_required` 세대 배치 재발급과 `Idempotency-Key` 보호
- reservation 해제와 참여자별 delete 발급 조건 적용
- 재일정 시작(기존 확정 유지), 대체 제안 확정 시 delete+create 동시 발급, 종단 실패 시 confirmed event 무변경
- 설계 7 T3 충돌 검사의 대체 대상 자기 제외와 snapshot `app_confirmed_event_id`
- 완료 기준: 취소 후 같은 시간 재확정이 가능하고, claim 이력이 있는 모든 사용자에게 delete가 발급되어 고아 이벤트가 남지 않는다

### W5. 관계 종료 handler

- membership 종료·강퇴·해산·계정 삭제의 command 종료와 reservation 해제
- 탈퇴자와 해산 Party 멤버 전원에게 tombstone + `calendar_cleanup_suggestions`
- `GET`/`DELETE /v1/calendar/cleanup-suggestions` 전달·소진 경로와 bootstrap 생존
- 계정 삭제는 설계 2 §10 worker 단계, 세션 폐기 선행으로 claim 틈 차단
- 완료 기준: 탈퇴·강퇴·해산은 종료 transaction 안에서, 계정 삭제는 세션 폐기 이후에 처리되어 어느 경로에서도 claim이 성립하지 않는다. 관계가 끝난 모든 사용자가 cursor 만료 후에도 잔존 일정 정리 경로를 받는다

### W6. iOS writer 경계

- `CalendarServing`을 권한·source·snapshot reader·command writer로 분리
- destination calendar 선택·생성과 로컬 opaque key, `destination_status` 반영
- 재설치·writer 전환 시 전 캘린더 범위 marker scan
- snapshot serializer의 `app_confirmed_event_id` allowlist 필드와 marker 파싱
- SwiftData 로컬 mapping, URL marker scan, create/delete 실행과 보고 큐
- 오프라인 보고 재전송과 `stale_lease` 복구
- 완료 기준: 재시도·재설치·기기 전환에서 EventKit 이벤트가 하나만 존재한다

### W7. iOS 화면과 복구 CTA

- 확정 카드의 본인 전용 반영 배지
- `failure_class`별 조치 화면과 재반영·재선택 CTA
- writer device 전환 제안, `GET/DELETE /v1/calendar/cleanup-suggestions` 기반 정리 목록
- 완료 기준: 다른 멤버 화면에 반영 상태 열이 존재하지 않고 본인 CTA로 모든 `failure_class`가 복구된다

### W8. E2E 인수 조건

- 3명 확정 → 반영 → 취소 → 삭제
- 권한 철회·destination 삭제·기기 전환 복구
- 재일정: 대체 제안 확정 시 캘린더 교체, 대체 제안 실패 시 원래 약속 보존
- privacy payload scan
- 완료 기준: `docs/calendar_write_design.md` §11 체크리스트 자동화

## 의존성

```text
상세 설계 9 완료
        │
P0/P1 + calendar C1~C3 + proposal Q1~Q3
        │
        └─▶ W0 ─▶ W1 ─▶ W2 ─▶ W3 ─┬─▶ W4 ─┐
                                    ├─▶ W5 ─┼─▶ W7 ─▶ W8
                                    └─▶ W6 ─┘
```

W4(취소·재일정), W5(관계 종료 handler), W6(iOS writer 경계)는 W3 완료 후 병렬로 진행할 수 있다.

W0의 순수 상태 테스트와 W6의 로컬 mapping 설계 초안은 서버 선행 작업과 병행할 수 있지만, 사용자 결정에 따라 실제 구현 착수는 상세 설계 9 완료 뒤로 제한한다.
