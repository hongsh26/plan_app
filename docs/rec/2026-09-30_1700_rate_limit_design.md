# 요청 제한 설계 작성

## 결정

P3에서 제외한 수락·미리보기 제한과 인증·계정 삭제 제한을 **공용 장치 하나**로 설계했다. 설계는 `docs/rate_limit_design.md`(번호 있는 상세 설계 1~11에 속하지 않는 횡단 인프라), 구현 계획은 `.omc/plans/rate-limit-implementation.md`(R0~R5).

- 저장소는 **PostgreSQL 고정 창 카운터**(`rate_limit_buckets`, FK 없음). 판정은 도메인 트랜잭션 **밖의** 자동 커밋 upsert이고, token 조회와 멱등성 replay 확인보다 먼저다.
- 클라이언트 IP는 `CLIENT_IP_SOURCE`(`remote_addr|xff`)와 `TRUSTED_PROXY_HOPS`로 `X-Forwarded-For` 오른쪽 N번째를 쓰고 IPv6는 /64로 묶는다. 원문 IP는 저장하지 않고 keyed hash만 쓴다.
- `429`는 `Retry-After`(창 종료까지 남은 초)만 담고 키·한도·남은 횟수는 담지 않는다.
- audit는 bucket 초과 순간 1줄만 남긴다. 미인증 로그인 실패 audit는 IP당 10분 5행, 전역 10분 300행으로 상한을 둔다.

## 탈락한 대안

| 대안 | 이유 |
|---|---|
| 프로세스 메모리 카운터 | API task가 2개 이상이라 실효 한도가 N배가 되고 배포마다 초기화된다. 보조 사전 차단 캐시로만 쓴다(DB가 초과를 확인한 키만 막는다) |
| Redis | 새 인프라·새 장애 지점. 저빈도 endpoint에는 DB upsert로 충분하다. 병목이 되면 재검토 |
| 도메인 트랜잭션 안에서 카운트 | token 추측 시도는 대부분 `404`로 롤백되어 카운트가 함께 사라진다. 제한이 무력화되므로 금지 불변식으로 못박았다 |
| sliding window 로그 | 요청마다 row와 정리 비용이 든다. 고정 창의 경계 burst(최대 2배)를 감수한다 |
| 초대 생성 제한을 공용 장치로 이관 | 초대 row가 남아 `created_at` count로 충분하다. 그대로 유지 |

## 사용자 확인이 필요한 것

- `auth.apple` 30/분, `auth.refresh` 300/분, `account.delete` 10/시간, audit 전역 300/10분은 **잠정값**이다. 근거 계측이 없다.
- CGNAT로 여러 사용자가 한 IP를 공유하는 경우의 IP 한도 충돌은 계측 후 조정한다.

## 독립 검토(architect) 1차와 반영

High 2건, Med 8건을 반영했다. 근본 원인은 두 가지였다: 캐시와 audit 트리거가 모두 "DB가 모든 시도를 본다"는 가정에 기댔고, XFF와 역할 분리를 확인 없이 "안전"·"기존 방식"이라고 단정했다.

- **H1 캐시가 초과 audit를 지움**: 캐시 차단을 "DB가 `> limit`을 반환한 키"로만 좁히고, `limit`에 도달한 다음 요청은 DB를 거쳐 `limit+1`을 받게 했다. 캐시로 막은 요청은 세지 않는다고 불변식 4를 고쳤다.
- **H2 XFF 폴백이 안전하다는 주장은 틀렸다**: N 과대 설정은 위조 우회, RemoteAddr 폴백은 전역 bucket 공유 가용성 공격이다. 폴백을 설정 오류(IP 키 제외 + 경보)로 재분류하고, `Header.Values` 결합·포트 파싱·`Unmap`·`CLIENT_IP_SOURCE` 명시 필수를 넣었다.
- **M1** 권한은 기존 default privileges를 따른다(좁히기는 SELECT 필요). **M2** audit 상한 키를 IP 단독으로 바꾸고 전역 상한을 추가했다. **M3** 웹 폴백을 이름 없는 정적 페이지로 degrade. **M4** refresh 300/분과 429는 세션 상실이 아니라는 클라이언트 계약. **M5** 장치 실패는 `503`, 전용 pool과 `statement_timeout`. **M6** 캐시 만료를 DB가 계산한 상대 시간으로. **M7** 삭제 한도 10/시간. **M8** 인수 조건과 계획 보강.
- backlog의 "요청 제한이 필요할 때 Valkey"를 이 결정으로 대체했다고 기록했다.

## 재검토(critic) 2차와 반영

REVISE 판정(High 1, Med 4). 1차 반영에서 새로 생긴 결함이었다.

- **폴백 시 IP 키 제외는 오히려 IP 전용 scope를 무제한으로 만들었다**: 공격자가 LB를 우회해 직접 붙으면 폴백을 일부러 일으킬 수 있다. `ip:fallback` 공유 subject로 세도록 바꾸고 SG 요구를 운영 조건에 넣었다.
- 미인증 scope 초과 audit는 IP 순환으로 행이 늘어나므로 지표만 남긴다. `auth.refresh`는 open(전면 503 방지). 다중 규칙 `Allow`와 limiter 오류 시 audit 생략을 명시했다.
- 웹 폴백은 설계 4 §9.1을 지금 개정했다: 초과·장치 실패·폴백 시 잘못된 token 페이지와 바이트 단위로 같은 이름 없는 정적 페이지.
- Low: 캐시 만료는 ms floor, 다중 task audit 테스트, refresh 시그니처 불변, audit 저장 형식(`target_type=scope`, `result=failure`).

## 상태

설계는 초안이다. 2차까지 반영했고 최종 확인 후 확정한다. 잠정 한도는 사용자 확인이 필요하다.
