# 요청 제한(rate limit) 설계

기준 설계: `docs/account_backend_design.md` §6 오류 코드·§11 보안·§12 토폴로지, `docs/party_membership_design.md` §4.2·§10, `docs/party_visibility_design.md` §5, `docs/availability_search_design.md`, `docs/proposal_lifecycle_design.md`

## 0. 설계 상태

- 상태: 초안. 독립 검토(architect) 1차 지적을 반영했다. **한도 숫자 중 "잠정" 표시는 운영 계측 전까지의 출발값이며 사용자 확인이 필요하다.** 번호 있는 상세 설계 1~11(`feature_design_backlog.md`)에 속하지 않는 횡단 인프라 설계다.
- 이 문서가 소유하는 것: 공용 제한 장치의 저장 방식, 판정 순서, 클라이언트 IP 규칙, `429` 응답 계약, audit·로그 규칙, 각 호출자의 적용 지점.
- 이 문서가 소유하지 않는 것: 가능 시간 검색·제안 API의 한도 숫자(각 설계가 소유하고 이 장치를 쓴다). 초대 **생성**의 시간당 10회 제한(§3.3).

이 설계가 필요한 이유는 P3에서 수락·미리보기 제한을 뺐기 때문이다. 초대 미리보기는 token 유효성을 알려주는 검증 경로이고(`party_membership_design.md` §4.2), 제한이 없으면 404 통일이 token 추측을 막지 못한다. 인증·계정 삭제 제한(`account_backend_design.md` §11)과 `audit_events`에 로그인 실패마다 행이 쌓이는 문제(`progressing.md`)도 같은 장치로 푼다.

## 1. 목표와 비목표

### 목표

1. 여러 API task에 걸쳐 **일관되게** 요청 횟수를 제한한다.
2. token 추측, 인증 남용, 계정 삭제 남용, 반복 probe를 억제한다.
3. 제한 초과 응답을 하나의 계약(`429 rate_limited` + `Retry-After`)으로 통일한다.
4. 제한 장치가 audit·로그·DB에 개인정보나 token을 남기지 않는다.

### 비목표

- DDoS 방어. 이는 load balancer/WAF의 몫이며 §8이 운영 요구로만 적는다.
- 사용자별 요금제 차등. 시스템 남용 방지 한도만 다룬다.
- 정밀한 sliding window. 고정 창의 경계 burst는 감수한다(§4.3).

## 2. 결정과 탈락한 대안

| 대안 | 판정 | 이유 |
|---|---|---|
| **PostgreSQL 고정 창 카운터** | 채택 | 이미 있는 유일한 공유 저장소다. 새 의존성·인프라가 없다. 대상 endpoint는 저빈도(인증, 초대, 삭제)라 요청당 upsert 1회가 감당된다 |
| 프로세스 메모리 카운터 | 탈락 | §12는 API task 2개 이상을 전제한다. 각 task가 한도만큼 통과시키므로 실효 한도가 N배가 되고 배포마다 초기화된다. 단, 보조 가속기로는 쓴다(§4.4) |
| Redis/Valkey 등 외부 저장소 | 탈락 | 새 인프라와 새 장애 지점이다. `feature_design_backlog.md`의 "요청 제한이 필요할 때 Valkey를 추가"를 **이 결정이 대체한다**(backlog에 반영). 트래픽이 늘어 DB upsert가 병목이 되면 재검토한다 |
| 도메인 트랜잭션 안에서 카운트 | **금지** | 토큰 추측 시도는 대부분 `404`로 끝나 트랜잭션이 롤백된다. 카운트도 함께 롤백되어 실패 시도가 세어지지 않는다 → 제한이 무력화된다(§4.2) |
| 행 개수 세기(`created_at` count) | 초대 생성에만 유지 | 초대 row가 어차피 남는 경우에만 성립한다. 실패 시도는 row를 남기지 않으므로 공용 장치로는 부적합하다 |
| 이동 창(sliding window) 로그 | 탈락 | 요청마다 row를 쌓아 쓰기량과 정리 비용이 크다 |

## 3. 적용 범위와 한도

한도는 **범위(scope)** 이름으로 식별하고, 한 요청이 여러 키(사용자, IP)를 가지면 **모두 증가시킨 뒤** 하나라도 초과면 거부한다.

### 3.1 범위 표

| scope | 키 | 창 | 한도 | 장치 실패 시 | 근거 |
|---|---|---:|---:|---|---|
| `auth.apple` | IP | 1분 | 30 (잠정) | closed | 미인증이라 IP뿐이다. Apple 서버 호출 전에 판정한다 |
| `auth.refresh` | IP | 1분 | 300 (잠정) | open | 256-bit token이라 추측은 비현실적이다. access token 수명 15분 동안 CGNAT IP 하나 뒤의 활성 기기를 수용해야 한다(§6의 클라이언트 계약 필수). 추측이 비현실적이라 남용 방지 가치가 낮고, 장치 실패로 전면 `503`이 되면 사실상 앱 장애이므로 open이다 |
| `account.delete` | 사용자 | 1시간 | 10 (잠정) | closed | 성공 후에는 계정이 잠겨 `Require`가 막으므로(423) 카운트되는 것은 400/409 실패 반복뿐이다. If-Match 낡은 409가 반복돼도 정당한 삭제가 막히지 않을 여유를 둔다 |
| `invite.preview` | 사용자 / IP | 1분 | 10 / 30 | closed | `party_membership_design.md` §4.2 |
| `invite.accept` | 사용자 / IP | 1분 | 5 / 30 | closed | 같은 문서 §4.2 |
| `invite.web` | IP | 1분 | 30 | degrade(이름 없는 정적 페이지) | 웹 폴백은 유효 token에 Party 이름을 보여주므로(설계 4 §9.1) 인증 없이 닿는 가장 약한 검증 경로다. 초과·장치 실패 시 **Party 이름 없는 정적 App Store 안내**로 떨어져 유효성을 드러내지 않는다 |
| `visibility.change` | 사용자+Party | 1분 | 10 | open | `party_visibility_design.md` §5 |
| `availability.search` | 사용자+Party, 사용자 전체(60/분) | 각 설계 | 각 설계 | open | 설계 6이 두 번째 키를 소유한다. 응답은 §6 계약 |
| `proposal.*` | 각 설계 | 각 설계 | 각 설계 | open | 설계 7이 소유 |
| `audit.auth_failure` | IP | 10분 | 5 | 해당 없음 | 미인증 실패 audit 행 상한(§7) |
| `audit.auth_failure.global` | 전역 | 10분 | 300 (잠정) | 해당 없음 | IP 순환 공격의 전체 행 수 상한(§7) |

- "실패 시"는 제한 장치 자체의 DB 문장이 실패했을 때의 동작이다(§4.5).
- IP가 키에 들어가는 항목은 모바일 통신사 CGNAT로 여러 사용자가 한 IP를 공유할 수 있다. 그래서 IP 한도는 사용자 한도보다 넉넉하게 잡는다. 인증은 사용자가 없으므로 30/분으로 시작하고 계측 후 조정한다.
- 사용자 키는 인증(`Require`)을 통과한 뒤에만 쓰므로 미로그인 요청은 `401 invalid_session`이 먼저다(`party_membership_design.md` §10).
- 키는 조회 전에 만들어지므로 임의 Party ID로 bucket row를 만들 수 있다(`visibility.change`). UUID 형식 검증만 통과하면 만들어지며 prune(§8)이 회수한다. 행 수는 요청 수에 선형이고 창이 짧아 상한이 있다.

### 3.2 다중 키 요청

`invite.accept`처럼 사용자·IP 두 키를 갖는 요청은 한 SQL 문으로 두 bucket을 함께 증가시킨다. `Limiter.Allow`는 서로 다른 한도·창을 가진 **여러 규칙(scope, 한도, 창, subject)을 한 호출로** 받으며(audit 이중 scope도 같은 방식), 교착을 막기 위해 VALUES를 `(scope, key_hash)` 순으로 정렬하고, 같은 scope 안의 subject는 `user:`/`ip:` 접두사로 네임스페이스를 나눠 두 키가 같은 row를 가리키는 "cannot affect row a second time" 오류를 막는다. 둘 중 하나라도 한도를 넘으면 거부하고, 거부된 시도도 카운트에 남는다. 반환하는 `Retry-After`는 초과한 키들의 최댓값이다.

### 3.3 이 장치를 쓰지 않는 제한

- **초대 생성 시간당 10회**: `party_invites.created_at` count로 유지한다(P3에서 구현). 초대 row가 항상 남고 실패 시도는 `403`/`409` 등 도메인 오류라 남용 표면이 다르다.
- Party 활성 초대 3개, 소유 Party 10개 등 **개수 한도**는 제한이 아니라 도메인 불변식이다.

## 4. 동작

### 4.1 저장 구조

```sql
CREATE TABLE rate_limit_buckets (
    scope        text        NOT NULL,
    key_hash     bytea       NOT NULL,
    window_start timestamptz NOT NULL,
    count        integer     NOT NULL,
    PRIMARY KEY (scope, key_hash, window_start)
);
CREATE INDEX rate_limit_buckets_window_start_idx ON rate_limit_buckets (window_start);
```

- **`users`나 다른 테이블로의 FK를 두지 않는다.** 그래서 `users → devices → sessions` 잠금 규약과 `users`-first 규약에 참여하지 않고, 사용자 삭제가 이 표에 막히지 않는다.
- `key_hash`는 `HMAC-SHA256(RATE_LIMIT_KEY, scope || 0x00 || subject)`다. 원문 IP·사용자 ID·Party ID를 저장하지 않는다. bucket은 최대 1시간만 살아 있으므로 키를 교체해도 손실이 없다.
- 창 경계와 현재 시각은 **DB 시각**으로 정한다: `window_start = date_bin(창, now(), 'epoch')`. API task 간 시계 오차가 창을 어긋나게 하지 못한다.

### 4.2 판정 순서와 불변식

다음은 모든 호출자가 지켜야 하는 불변식이다.

1. **제한 판정은 도메인 트랜잭션 밖에서, 별도 자동 커밋 문장으로 실행한다.** 이후 도메인 처리가 롤백돼도 카운트는 남는다.
2. **제한 판정은 `Require` 다음, 다른 모든 입력 검증과 token/자원 조회보다 먼저다.** 단 scope 키를 만들기 위한 경로 매개변수 파싱(`visibility.change`의 Party UUID 등)은 예외로 먼저 한다. token 길이 검사(`invites.go`의 형식 404)도 판정 뒤로 옮긴다. 잘못된 token이 `404`로 끝나는 시도도 모두 센다.
3. **제한 판정은 멱등성 replay 확인(`mutation.Run`)보다 먼저다.** 같은 `Idempotency-Key` 재전송도 한도를 소비한다. 정상 클라이언트의 재시도는 한도에 여유가 있게 설계한다(수락 5/분은 재시도를 포함해도 충분하다).
4. **DB에 도달한 시도는 거부돼도 카운트한다.** 한도를 넘긴 뒤에도 창이 비지 않는다. 단 §4.4의 프로세스 내 사전 차단으로 DB에 가지 않은 요청은 이미 초과 상태이므로 세지 않는다.
5. 한도 초과 응답은 도메인 조회를 하지 않는다. 존재 여부에 대한 어떤 정보도 담지 않는다.

```sql
INSERT INTO rate_limit_buckets AS b (scope, key_hash, window_start, count)
VALUES ($1, $2, date_bin($3::interval, now(), 'epoch'), 1)
ON CONFLICT (scope, key_hash, window_start)
DO UPDATE SET count = b.count + 1
RETURNING count, window_start;
```

`count > limit`이면 초과다. 남은 초는 같은 문장에서 DB가 계산해 함께 돌려준다: `RETURNING count, ceil(extract(epoch FROM (window_start + $3::interval - now())))::int AS retry_after`. 애플리케이션 시계를 쓰지 않는다.

창 계산에 `date_bin`을 쓰므로 PostgreSQL 14 이상이 필요하다(로컬 18.2, 관리형 DB의 최소 버전을 14 이상으로 고정한다).

### 4.3 고정 창의 한계

창 경계에서 최대 한도의 2배가 한 창 길이 안에 통과할 수 있다. 대상은 token 추측 억제이지 정밀 과금이 아니므로 감수한다. 더 좁혀야 하면 창을 나눠 이중 scope(1분 + 1시간)를 쓴다.

### 4.4 프로세스 내 사전 차단

요청 폭주 시 DB 쓰기가 늘지 않도록 각 task가 **읽기 전용 가속기**를 둔다.

- task는 DB가 **초과(`count > limit`)를 반환한** 키에 대해서만 `(scope, key_hash) → 만료 시각`을 메모리에 둔다. 만료 시각은 `local_now + floor(남은 시간, ms)`로 저장한다(헤더의 `Retry-After`만 올림 초). 올림 초로 저장하면 창 종료 뒤에도 최대 1초+RTT 동안 오거부가 난다. 남은 시간은 DB가 ms 단위로 함께 돌려준다. 절대 시각을 비교하지 않으므로 task와 DB의 시계 오차가 창 경계 오거부를 만들지 못한다.
- 만료 전에는 DB에 가지 않고 즉시 `429`를 낸다. 이미 DB가 초과를 확인한 창이므로 정당한 요청을 잘못 거부하지 않는다.
- `count == limit`을 받은 요청 다음에는 캐시가 없다. 다음 요청은 DB로 가서 `limit + 1`을 받는다. 그래서 **초과 audit(§7)의 `limit + 1` 순간은 항상 DB를 거치며, 창당 한 번만 발생한다.**
- 캐시 항목이 없거나 만료되면 항상 DB 문장을 실행한다. 캐시는 통과를 허용하지 않는다.
- 크기 상한(예: 항목 10,000)과 만료 제거로 메모리를 제한한다. task 재시작은 캐시를 잃을 뿐 정확성에 영향이 없다.

### 4.5 제한 장치 자체의 실패

DB 문장이 실패(타임아웃, 연결 오류)했을 때 scope별로 다르게 처리한다.

- **closed** (`auth.apple`, `account.delete`, `invite.preview/accept`): 요청을 `503`과 `Retry-After`로 거부한다. 코드베이스는 가용성 실패를 `503`으로 응답한다(`auth/http.go`, code는 `CodeInternal`). 남용 표면이라 제한 없이 통과시키지 않는다.
- **open** (`auth.refresh`, `visibility.change`, 검색·제안): 통과시키고 `result=limiter_error` 로그·지표를 남긴다.
- **degrade** (`invite.web`): 초과·장치 실패·폴백 모두 이름 없는 정적 페이지를 반환한다(§3.1의 "degrade" 표기).
- IP를 순환하는 `auth.apple` 폭주는 키가 매번 새로워 캐시가 막지 못하고 전용 pool을 포화시킬 수 있다. 이때 closed scope(`auth.apple`, `invite.*`, `account.delete`)는 `503`이 된다. 대량 트래픽 방어는 WAF의 몫이며(§8) 이 의존을 명시한다.
- 제한 장치는 **전용 작은 pool**(예: 최대 4연결)을 쓰고 연결 파라미터로 `statement_timeout`(잠정 150ms)을 둔다. ctx timeout은 그보다 약간 긴 250ms(잠정)다. pgx는 ctx 초과 시 연결을 끊으므로, 도메인 pool(기본 10)을 잠식하고 재연결이 폭증하는 것을 격리한다. pool acquire 대기도 ctx 안에 포함된다. 근거는 일반 API p95 300ms 목표(`account_backend_design.md` §12)의 일부만 쓰기 위해서다.
- **timeout이 났어도 서버에서는 증가가 커밋됐을 수 있다.** 카운트가 한 번 더 세어질 수 있음을 감수한다(더 엄격한 방향).
- 제한 문장이 실패하는 상황은 대개 DB 자체가 불안한 상황이다. `limiter_error`가 지속되면 경보한다.

## 5. 클라이언트 IP

load balancer 뒤에서 `RemoteAddr`는 balancer 주소라 IP 한도가 전역 하나로 뭉친다.

### 5.1 설정

- `CLIENT_IP_SOURCE`: `remote_addr` 또는 `xff`. `APP_ENV != local`에서는 **명시 필수**다(기본값 없음). NLB·클라이언트 IP 보존 구성은 `remote_addr`, ALB·CloudFront 등 프록시 구성은 `xff`를 쓴다.
- `TRUSTED_PROXY_HOPS`: `xff`일 때 1 이상 정수 필수. 우리 인프라의 프록시 개수다(ALB만이면 1, CloudFront+ALB면 2).
- 로컬은 기본 `remote_addr`다. `RATE_LIMIT_KEY`도 로컬에서는 고정 기본값을 써서 "언제든 기존 코드를 돌릴 수 있어야 한다"를 지킨다. 비로컬은 필수다.

### 5.2 파싱 규칙

- `X-Forwarded-For`는 `r.Header.Values`로 **모든 줄을 순서대로 합쳐** 쉼표로 나눈다. `Header.Get`은 첫 줄만 돌려주므로 클라이언트가 보낸 줄만 읽는 위조 경로가 생긴다.
- **오른쪽에서 N번째** 항목을 클라이언트 IP로 쓴다. 왼쪽 항목은 위조 가능하다.
- 항목은 `ip`, `ip:port`, `[v6]:port` 형식을 모두 파싱한다. 실패하면 파싱 실패다.
- IPv4-mapped IPv6(`::ffff:a.b.c.d`)는 `Unmap()`해 IPv4로 다룬다. 하지 않으면 /64로 자를 때 모든 IPv4 클라이언트가 한 bucket으로 뭉친다.
- IPv4는 `/32`, IPv6는 `/64` 단위로 묶는다.
- 원문 IP는 저장하지 않고 §4.1의 keyed hash로만 쓴다. 로그에도 남기지 않는다.

### 5.3 폴백은 설정 오류다

헤더가 없거나 항목이 N개 미만이거나 파싱에 실패한 경우는 **"안전한 대체"가 아니라 설정 오류**다.

- N을 실제 프록시 수보다 크게 잡으면 오른쪽 N번째 항목이 클라이언트가 쓴 값이 되어 공격자가 자기 bucket을 고른다(우회).
- `RemoteAddr`로 떨어지면 모든 사용자가 balancer bucket 하나를 공유해 한 명이 전체를 막을 수 있다(가용성 공격).
- 폴백 요청은 IP 키를 빼지 않고 scope별 **공유 subject `ip:fallback`** 하나로 센다. 이 bucket은 폴백 경로로 들어온 트래픽끼리만 공유하므로 정상 경로(XFF가 올바른 요청)에 영향이 없고, IP만 키로 쓰는 scope(`auth.apple`, `auth.refresh`, `invite.web`)가 폴백 때 무제한이 되지 않는다. 공격자가 LB를 우회해 직접 붙어 폴백을 일부러 일으켜도 한 bucket에 갇힌다. 오류 로그와 `client_ip_fallback` 지표를 남기고 경보한다.
- `invite.web`은 폴백 요청이면 항상 이름 없는 정적 페이지를 낸다.
- 운영 요구: ALB·task는 CloudFront 등 정해진 상위 계층에서만 접근 가능해야 한다(보안 그룹). 그렇지 않으면 직접 접근으로 XFF 항목 수가 달라진다.
- 배포별 확인 사항: ALB는 기본 append 모드에서 연결 IP를 오른쪽에 붙이지만 preserve/remove 모드와 client-port 옵션에서 형식이 다르다. 배포 시 실제 헤더로 N을 검증한다.

## 6. `429` 응답 계약

```text
HTTP/1.1 429 Too Many Requests
Retry-After: <초, 정수, 최소 1>
{"code":"rate_limited","message":"요청이 너무 많다","request_id":"..."}
```

- 모든 scope가 같은 코드·형식을 쓴다. 본문에 어느 키(사용자/IP)가 초과했는지, 남은 횟수, 한도 값을 담지 않는다. 담으면 다른 키의 상태를 알려주는 oracle이 된다.
- `Retry-After`는 **창 종료까지 남은 초**다. 검색·제안의 "aggregate retry-after"는 사용자·Party 단위 창의 종료 시각이므로 개별 조건이나 결과를 드러내지 않는다.
- 응답 헤더 `X-RateLimit-*`는 제공하지 않는다.
- 클라이언트는 `Retry-After` 이후에만 재시도한다. 수락·미리보기는 자동 재시도하지 않고 사용자 동작으로만 다시 보낸다.
- `server/openapi/openapi.yaml`에 공통 `429` 응답과 `Retry-After` 헤더를 추가한다.
- **클라이언트 계약: `/v1/auth/refresh`의 `429`는 세션 상실이 아니다.** 로그아웃하거나 refresh token을 폐기하지 않고 `Retry-After` 이후 재시도한다. 이를 세션 상실로 처리하면 한도 초과가 대량 로그아웃으로 번진다.
- `503`(장치 실패, closed)도 같은 방식으로 재시도한다. 본문 code는 기존 가용성 실패와 같은 값(`auth/http.go`의 `CodeInternal`)이고 `Retry-After`는 1초(잠정)다.

## 7. audit, 로그, 개인정보

- **요청마다 audit를 남기지 않는다.** 제한 장치가 쓰기 증폭기가 되면 안 된다.
- bucket이 처음 초과되는 순간(`RETURNING count = 한도 + 1`, §4.4 덕에 창당 정확히 한 번 DB를 거친다)에만 `rate_limit.exceeded` audit 한 줄을 남긴다. action에는 scope를 담고 IP·token·`key_hash`는 담지 않는다(`audit_events`에 자유 텍스트 열이 없고, 원문에서 파생된 값을 남기지 않는다). 저장 형식은 `action='rate_limit.exceeded'`, `target_type=<scope>`, `result='failure'`다(`audit_events`에는 자유 텍스트 열이 없다). **사용자가 인증된 scope(`invite.*`, `account.delete`, `visibility.change` 등)에만** audit를 남기고 actor를 담는다. 미인증 scope(`auth.*`, `invite.web`)는 IP를 순환하면 행이 요청량에 비례해 늘어나므로 audit 없이 지표만 올린다. 한 요청에서 사용자·IP bucket이 동시에 초과 전이되면 audit는 그 요청당 1줄이다. 상관 분석은 `request_id`와 scope별 지표로 한다.
- **재귀 방지**: `audit.auth_failure*`의 초과는 audit를 남기지 않고 지표만 올린다.
- **limiter 오류 시**: 실패 audit 상한 판정이 오류면 audit를 쓰지 않고 지표만 올린다(쓰기 증폭 방지가 audit 보존보다 우선).
- **미인증 실패 audit(`auth.apple_sign_in`, `auth.apple_code_exchange`, `auth.apple_code_subject_mismatch`)의 상한**: 지금은 실패마다 `audit_events`에 행이 쌓인다(`internal/auth/service.go` `auditFailure`). 실패 audit를 쓰기 전에 두 scope를 모두 통과해야 한다: IP 단독 `audit.auth_failure`(action과 무관하게 IP당 10분 5행)와 전역 `audit.auth_failure.global`(10분 300행). IP를 순환하는 공격(IPv6 /48 하나가 /64 65,536개)도 전역 상한이 전체 행 수를 막는다. 한도 초과분은 행을 쓰지 않고 지표만 올린다. **공격 중에는 정상 실패 audit가 일부 누락될 수 있다**는 대가를 감수한다.
- 구현: `auditFailure` 호출 지점은 `auth/service.go`의 Apple 로그인 경로 4곳(137, 155, 168, 274)이며 274는 Apple 성공 뒤 423 경로도 같은 상한을 따른다. refresh 경로는 `auditFailure`를 부르지 않으므로 시그니처를 바꾸지 않는다. `SignInInput`에 클라이언트 IP(hash)와 `Limiter`를 주입한다.
- 로그에는 `scope`, `result=limited|limiter_error`, `request_id`만 남긴다. IP·hash·사용자 ID는 남기지 않는다.
- 지표: scope별 `limited` 수, `limiter_error` 수, `client_ip_fallback` 수. 초대 token 추측 의심은 `invite.*` scope의 `limited` 급증으로 본다.
- `RATE_LIMIT_KEY` 교체: bucket이 최대 1시간만 살아 있어 교체해도 손실이 없다. 다만 롤링 배포 중에는 옛 키와 새 키가 함께 쓰여 실효 한도가 최대 2배가 될 수 있다.

## 8. 정리와 운영

- scheduler 작업 `rate_limit_prune`이 `window_start < now() - 2h`인 행을 배치로 삭제한다. 가장 긴 창이 1시간이므로 2시간이면 살아 있는 창을 지우지 않는다.
- 권한은 기존 `01-roles.sql`의 default privileges(api·worker에 CRUD)를 그대로 따른다. `INSERT ... ON CONFLICT ... RETURNING`과 `DELETE ... WHERE`는 SELECT 권한도 필요하므로 INSERT/UPDATE만 또는 DELETE만 주는 좁히기는 하지 않는다. 좁히려면 migration에서 REVOKE하고 `server/test/role_separation_test.go`를 함께 바꾼다. prune을 실행하는 scheduler 프로세스가 쓰는 DB 역할은 구현(R5)에서 확인한다.
- 운영 요구: load balancer/WAF가 IP당 대략적인 초당 요청 상한을 별도로 둔다. 이 장치는 endpoint 의미 단위 제한이고 대량 트래픽 방어가 아니다.
- 새로 필요한 설정: `RATE_LIMIT_KEY`(secrets manager, api 역할), `CLIENT_IP_SOURCE`, `TRUSTED_PROXY_HOPS`. 제한 장치 전용 pool 크기와 `statement_timeout`.

## 9. 적용 지점

| 호출자 | 위치 | 비고 |
|---|---|---|
| `POST /v1/auth/apple`, `/refresh` | handler 진입 직후, 요청 본문 파싱·Apple 호출 전 | IP만. 미인증 실패 audit는 §7의 두 scope를 따른다 |
| `DELETE /v1/me` | `Require` 이후, `mutation.Prepare` 이전 | 사용자. `account` 패키지가 소비자 interface를 둔다 |
| `GET /v1/invites/{token}/preview` | `Require` 이후, token 길이 검사·조회 이전 | 사용자+IP |
| `POST /v1/invites/{token}/accept` | `Require` 이후, `mutation.Prepare`·`mutation.Run`·token 길이 검사 이전 | 사용자+IP |
| 웹 폴백 `/i/{token}` | 페이지 핸들러 진입 직후 | IP |
| 공개 수준 변경 | 설계 5 구현 시 | 사용자+Party |
| 검색·제안 | 설계 6·7 구현 시 | 각 설계의 한도 |

구현은 handler에서 호출하는 `ratelimit.Limiter.Allow(ctx, Check{Scope, Subjects...})` 형태의 소비자 쪽 interface로 둔다(`Pinger`가 제공자 쪽에 있어 지적받은 것과 같은 실수를 반복하지 않는다).

## 10. 테스트 가능한 인수 조건

- [ ] 잘못된 token으로 미리보기를 분당 10회 넘게 호출하면 `429 rate_limited`와 `Retry-After`가 나온다. **이 시도들은 모두 `404`였으며 도메인 롤백 후에도 카운트가 남았다.**
- [ ] 수락이 사용자당 분당 5회, IP당 분당 30회를 넘으면 `429`다.
- [ ] 사용자 A가 한도를 소진해도 사용자 B의 같은 scope 요청은 통과한다(IP 한도 이내).
- [ ] 같은 `Idempotency-Key` 재전송도 한도를 소비한다.
- [ ] 두 API task(두 `Limiter` 인스턴스)가 같은 DB를 쓰면 합산 한도가 지켜진다.
- [ ] 창이 지나면 카운트가 새로 시작하고 `Retry-After`가 창 종료까지 남은 초와 일치한다.
- [ ] 로컬 사전 차단 캐시가 있어도 한도 미만 요청을 거부하지 않는다. 캐시는 DB가 초과를 반환한 키만 막고, 창이 바뀌면(DB 기준 남은 초 경과) 다시 통과한다.
- [ ] 한도에 딱 도달한 요청 다음 요청은 DB를 거쳐 `limit + 1`을 받고, 캐시 때문에 초과 audit가 사라지지 않는다. 캐시를 가진 두 `Limiter`가 같은 DB를 써도 초과 audit는 한 줄이다.
- [ ] 폴백(`ip:fallback`) 요청도 IP만 쓰는 scope의 한도를 소비하고, 폴백 때문에 정상 경로 요청이 거부되지 않는다.
- [ ] `invite.web` 폴백·초과·장치 실패 페이지는 잘못된 token 페이지와 **바이트 단위로 같고** Party 이름·유효성 신호가 없다.
- [ ] 미인증 scope(`auth.*`, `invite.web`)의 초과는 audit 행을 늘리지 않는다. 한 요청에서 두 bucket이 동시에 초과 전이돼도 audit는 1줄이다.
- [ ] limiter 오류일 때 실패 audit를 쓰지 않고 지표만 올린다.
- [ ] `xff` 모드에서 `X-Forwarded-For`의 왼쪽 위조 항목이 무시된다. 여러 줄 헤더는 모두 합쳐 파싱한다. `ip:port`, `[v6]:port`, IPv4-mapped IPv6가 올바르게 처리된다. 항목 부족·파싱 실패 시 IP 키가 판정에서 제외되고 `client_ip_fallback`이 기록된다. `APP_ENV != local`에서 `CLIENT_IP_SOURCE`가 없으면 기동을 거부한다.
- [ ] `auth.apple`·`auth.refresh`·`account.delete`가 한도 초과 시 `429`다. 초과한 `auth.apple` 요청은 Apple 서버(mock)를 호출하지 않는다.
- [ ] 초대 token 길이 검사 실패(형식 404)도 한도를 소비한다.
- [ ] IPv6 /64 안의 다른 주소가 같은 bucket을 쓴다.
- [ ] `rate_limit_buckets`, audit, 로그 어디에도 원문 IP·token이 없다.
- [ ] 초과 순간에 audit `rate_limit.exceeded`가 창당 1줄만 남고, 이후 거부는 audit를 늘리지 않는다.
- [ ] 같은 IP에서 로그인 실패를 100번 반복해도 action이 달라도 미인증 실패 audit 행은 10분에 5개 이하다. 서로 다른 IP 1000개에서 실패해도 전역 상한(300) 이하다. 이 두 scope의 초과는 `rate_limit.exceeded` audit를 남기지 않는다.
- [ ] `invite.*` scope의 초과 audit `rate_limit.exceeded`가 창당 1줄이다.
- [ ] 제한 문장이 실패하면 closed scope는 `503`과 `Retry-After`, open scope는 통과하며 `limiter_error`가 기록되고, `invite.web`은 이름 없는 정적 페이지다.
- [ ] 제한 장치의 pool 고갈·timeout이 도메인 pool을 잠식하지 않는다.
- [ ] 다중 키 upsert가 subject 접두사 덕에 같은 row 충돌 오류를 내지 않고, 정렬된 순서로 교착하지 않는다.
- [ ] 만료된 bucket을 prune 작업이 지우고 살아 있는 창은 지우지 않는다.

## 11. 현재 구현과의 차이

- 요청 제한 코드가 없다. `httpapi.CodeRateLimited` 상수와 P3의 초대 생성 시간당 count만 있다.
- `auditFailure`는 실패마다 `audit_events`에 행을 쓴다.
- 클라이언트 IP를 읽는 코드가 없다.

## 12. 미결정 사항

- `auth.apple`·`auth.refresh`·`account.delete`의 한도 숫자(잠정). 내부 Alpha 계측 후 조정하며 변경 이유를 기록한다.
- DB upsert가 병목이 되는 트래픽 규모. 넘으면 Redis 등을 별도 결정으로 재검토한다.
- 같은 사용자가 여러 기기에서 auth를 반복하는 정상 패턴의 IP 한도 충돌(CGNAT). 계측 후 IP 한도를 올리거나 기기 ID를 두 번째 키로 쓸지 정한다.
- /56~/48을 가진 공격자는 /64 키보다 훨씬 많은 bucket을 얻는다. 필요하면 /48 보조 키를 둔다.
- 웹 폴백 정적 페이지 본문은 AASA·웹 폴백 작업에서 확정한다. 설계 4 §9.1은 이 설계에 맞춰 개정했다.

## 13. 구현 순서

`.omc/plans/rate-limit-implementation.md`를 따른다. 초대 수락·미리보기가 첫 소비자다.
