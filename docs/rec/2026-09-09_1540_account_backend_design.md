# 계정·백엔드·데이터 소유권 설계 완료

## 확정 내용

- Go 모듈러 모놀리스와 PostgreSQL을 자체 백엔드 기준으로 확정했다.
- REST/OpenAPI, 사용자별 cursor 증분 동기화, SwiftData 로컬 투영, APNs invalidation 구조를 확정했다.
- access/refresh token, 기기별 세션, refresh 회전·재사용 탐지와 계정 삭제 상태를 설계했다.
- 모든 mutation에 멱등성 키와 version 충돌 검사를 적용한다.
- 도메인 상태, sync change와 outbox/job을 하나의 PostgreSQL transaction으로 커밋한다.
- 캘린더 원문은 기기에 두고 서버에는 최소 일정 사실과 Party별 공개 투영만 저장한다.
- Apple Calendar 실제 쓰기는 EventKit 권한을 가진 지정 iOS 기기가 수행하고, 서버는 명령·lease·결과 상태만 관리한다.
- Apple authorization code 교환과 provider refresh token 암호화/revoke를 계정 생명주기에 포함했다.

## 산출물

- 상세 설계: `docs/account_backend_design.md`
- 구현 계획: `.omc/plans/account-backend-implementation.md`

## 검증 기준

- 인증, 세션, 동기화, 오프라인, 삭제, 권한, 작업 재시도에 측정 가능한 인수 조건을 작성했다.
- 공식 Apple, Go와 PostgreSQL 문서 링크를 설계서에 남겼다.
- 별도 architect와 reviewer가 설계의 일관성과 구현 가능성을 검증한다.
- 최초 검토에서 EventKit의 기기 권한 경계, 원본 identifier 유출 가능성, Apple token revoke, device 신뢰, bootstrap과 DB 제약을 보완하도록 요청했다.
- EventKit device command 모델, 로컬 opaque key, server-issued device ID, snapshot bootstrap, partial index와 최소 멱등성 결과 보존으로 수정했다.
- 재검토에서 승인됐으며, 비차단 제안에 따라 캘린더 명령의 동일 트랜잭션 불변식과 claim/완료/실패 운영 지표도 추가했다.

## 다음 작업

- 캘린더 권한·읽기·동기화 상세 설계
- Party 생성·초대·역할·탈퇴 상세 설계
