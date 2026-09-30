# 요청 제한 구현 계획

기준 설계: `docs/rate_limit_design.md`
첫 소비자: `docs/party_membership_design.md` §4.2 초대 수락·미리보기

## 전제

- 서버(Go + PostgreSQL)만 다룬다. 새 의존성 없음(HMAC·sha256은 표준 라이브러리).
- 브랜치: 설계 문서는 `feature/rate-limit-design`, 구현은 설계 확정 후 새 `feature/rate-limit`. 완료 후 `main`에 fast-forward.
- 각 단계는 기존 API를 깨지 않고 테스트를 통과해야 한다.

## 단계

### R0. 설정과 클라이언트 IP

- `config`에 `RATE_LIMIT_KEY`(최소 32바이트, 로컬 고정 기본값, 비로컬 필수), `CLIENT_IP_SOURCE`(`remote_addr|xff`, 비로컬 필수), `TRUSTED_PROXY_HOPS`(xff일 때 1 이상), limiter 전용 pool 크기·`statement_timeout` 추가.
- `httpapi.ClientIP`: `Header.Values`로 모든 XFF 줄 결합, 오른쪽 N번째, `ip`/`ip:port`/`[v6]:port` 파싱, `Unmap`, IPv6 /64. 실패는 폴백=설정 오류(IP 키 제외, 로그·`client_ip_fallback` 지표).
- 완료 기준: 위조 좌측 항목 무시, 다중 줄 헤더, 포트·mapped 주소, 폴백 요청이 `ip:fallback` bucket으로 세어지고 IP만 쓰는 scope가 무제한이 되지 않음, IPv6 /64 동일 bucket, `CLIENT_IP_SOURCE` 누락 시 기동 거부 테스트.

### R1. 스키마와 Limiter

- migration `rate_limit_buckets`(FK 없음, `window_start` index). 권한은 기존 default privileges를 따른다(좁히려면 REVOKE + `role_separation_test.go` 갱신). PostgreSQL 14 이상 확인.
- `internal/platform/ratelimit`: `Limiter.Allow(ctx, rules ...Rule{Scope, Limit, Window, Subjects})`. 여러 규칙을 한 호출·한 SQL 문으로 다중 키 upsert(VALUES 정렬, `user:`/`ip:` 접두사), `date_bin`과 DB `now()`, `RETURNING count, retry_after`, 전용 작은 pool + `statement_timeout`, scope별 closed(503)/open/degrade, 프로세스 내 사전 차단 캐시(DB가 `> limit`을 반환한 키만, 만료는 `local_now + retry_after`, 항목 상한).
- 소비자 쪽 interface는 사용하는 패키지(`party`, `auth`, `account`)에 둔다.
- 완료 기준: 설계 §10의 단위·통합 항목(두 인스턴스 합산, 창 전환, `Retry-After`, 캐시가 정당한 요청을 거부하지 않음, `limit`+1 순간 DB 도달, 실패 모드, pool 격리, 다중 키 충돌 없음).

### R2. 429 계약

- 공통 `writeRateLimited`(`Retry-After` 정수 초, 본문에 키·한도·남은 횟수 없음), `openapi.yaml`에 429 응답과 헤더 추가.
- `/v1/auth/refresh` 429는 세션 상실이 아니라는 클라이언트 계약을 `openapi.yaml`과 iOS 문서에 적는다. `503`도 `Retry-After`를 담는다.
- 완료 기준: 계약 테스트, 본문에서 oracle 정보가 없음.

### R3. 초대 수락·미리보기 적용

- `Require` 이후, token 길이 검사·token 조회·`mutation.Prepare`·`mutation.Run` 이전에 `invite.preview`/`invite.accept` 사용자+IP 확인(형식 404 검사는 판정 뒤로 옮긴다).
- 완료 기준: 설계 §10의 첫 세 항목(잘못된 token 404 반복 → 429 + 롤백 후에도 카운트 유지, 수락 사용자·IP 한도, 사용자 격리)과 같은 `Idempotency-Key` 재전송이 한도를 소비. `party_membership_design.md` §10의 미리보기 429 체크박스 충족. 초대 scope 초과 audit 창당 1줄, 형식 404도 한도를 소비.

### R4. 인증·계정 삭제 적용과 audit 상한

- `auth.apple`·`auth.refresh`(IP), `account.delete`(사용자) 적용. Apple 호출 전에 판정.
- `auth.Service`에 `Limiter`를 주입하고 `SignInInput`에 클라이언트 IP(hash)를 추가한다. `auditFailure` 호출 지점은 Apple 로그인 경로 4곳(137·155·168·274)이며 refresh는 바꾸지 않는다. `auth.refresh`는 장치 실패 시 open.
- `auditFailure`를 `audit.auth_failure`(IP 단독, 10분, 5)와 `audit.auth_failure.global`(10분, 300)로 감싼다. 초과분은 행을 쓰지 않고 지표만 올린다. 이 두 scope 초과는 `rate_limit.exceeded` audit를 남기지 않는다.
- 완료 기준: 같은 IP 실패 100회(action 혼합)에도 audit 행 ≤ 5, 다른 IP 1000개에서도 ≤ 300, Apple mock 호출 0회(초과 시), 로그·audit에 IP·token 없음.

### R5. 정리 작업과 운영

- scheduler 작업 `rate_limit_prune`(2시간 초과 배치 삭제, 실행 DB 역할 확인)과 지표(`limited`, `limiter_error`, `client_ip_fallback`).
- `bucket` 테이블·audit·로그에 원문 IP·token이 없는 것을 검증하는 테스트. 웹 폴백 페이지는 별도 작업(`invite.web`)에서 붙인다.
- `docs/progressing.md`의 남은 항목 갱신, 설계 4 §10 체크박스 반영.
- 완료 기준: prune이 살아 있는 창을 지우지 않는 테스트.

## 이후 소비자

공개 수준 변경(설계 5), 가능 시간 검색(설계 6), 제안(설계 7) 구현 시 같은 `Limiter`를 호출한다. 웹 폴백 `invite.web`은 AASA·웹 폴백 작업에서 붙인다.
