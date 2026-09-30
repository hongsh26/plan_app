# 요청 제한 구현 (R0~R5)

설계: `docs/rate_limit_design.md`, 계획: `.omc/plans/rate-limit-implementation.md`. 브랜치 `feature/rate-limit`.

## 구현

- 설정(`config/ratelimit.go`): `RATE_LIMIT_KEY`(로컬 고정 기본값, 비로컬 필수), `CLIENT_IP_SOURCE`(비로컬 명시 필수), `TRUSTED_PROXY_HOPS`, 전용 pool 크기·`statement_timeout`. `.env.example`에 문서화.
- 클라이언트 IP(`httpapi/clientip.go`): XFF 모든 줄 결합·오른쪽 N번째·`ip:port`·`Unmap`·IPv6 /64. 해석 실패는 설정 오류이며 `ip:fallback` 공유 subject로 센다.
- `ratelimit.Limiter`: 여러 규칙 한 SQL upsert(정렬), DB `date_bin`/`now()`, HMAC key_hash, DB가 초과를 반환한 키만 막는 캐시(만료는 DB가 준 남은 ms의 상대 시간), closed(503)/open 모드, 전용 pool. migration `00011_rate_limit_buckets`(FK 없음).
- 적용: `/v1/auth/apple`(closed, Apple 호출 전), `/v1/auth/refresh`(open), `DELETE /v1/me`(closed), 초대 미리보기·수락(closed, 사용자+IP, token 길이 검사 앞). 인증된 scope만 `rate_limit.exceeded` audit(요청당 1줄), 미인증은 지표만.
- `auditFailure`: IP당 10분 5행 → 전역 10분 300행 상한. 초과분과 limiter 오류 시 행을 쓰지 않는다.
- scheduler `rate_limit_prune`(10분 주기, 2시간 초과 배치 삭제, worker 역할). OpenAPI에 429/503과 `Retry-After`.

## 검증

- `go vet`, `gofmt`, 실제 PostgreSQL로 `go test -count=1 ./...` 3회 통과, skip 0건. ratelimit·party·account는 `-race -count=2` 통과.
- 잘못된 token 미리보기 404 10회 뒤 429(롤백 후에도 카운트 유지), 형식 404도 소비, 같은 Idempotency-Key 재전송도 소비, 사용자 격리·IP 공유, 두 Limiter 합산과 초과 보고 1회, 캐시가 창 전환 뒤 거부하지 않음, auth.apple 초과 시 Apple 호출 0회, 실패 audit IP 5행·전역 상한, 장치 실패 시 closed=503/refresh open, bucket·로그에 원문 IP 없음, prune이 2시간 안쪽 row를 남김.
- 테스트는 무작위 HMAC 키로 bucket을 격리하고 horizon에 의존하지 않는다.

## 설계와 다르게 한 결정

- 캐시 만료 시각을 로컬 시계 절대값이 아니라 `local_now + floor(DB 남은 ms)`로 뒀다(설계 §4.4와 같은 방향의 구현 상세).
- `auditFailure`는 IP 규칙을 통과한 요청만 전역에 센다(전역 상한이 실제로 쓰인 행 수를 막게 하려는 순서).
- 한도는 `limits.go`의 var라 테스트가 덮어쓸 수 있다. 운영 값 변경은 이 파일 한 곳이다.

## 남은 것

- `invite.web`(AASA·웹 폴백), 공개 수준·검색·제안 소비자, CLIENT_IP hops 배포 검증, 경보 규칙.

## 독립 재검증과 반영

verifier: High 0, Med 1, Low 6. 코드 경로·불변식·Limiter 정확성·IP 처리·audit·권한·CI 기동 거부 스텝에는 결함이 없었다.

- Med(설계 문서 §0·§10·§11 정합) 반영: 구현·검증된 인수 조건을 체크하고 §11을 "구현됨/남은 차이"로 고쳤다.
- 테스트 공백 보강: 제한 장치 오류 시 실패 audit 생략(`auth/ratelimit_test.go`), 전용 pool의 `statement_timeout`(`postgres/limiter_pool_test.go`).
- 고정 창 경계에서 flaky할 수 있어 `awaitSafeWindow`로 경계 직전이면 지나가길 기다린다.
- 관측은 로그 기반이라는 점을 설계 §7에 명시했다. 캐시 포화 시 O(n) 스캔, 초대 endpoint의 OpenAPI 등재, pool 부하 격리 검증은 후속으로 남긴다.
