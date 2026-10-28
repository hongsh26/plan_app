# 공개 수준 서버 API와 details projection 구현

## 기능 구현 완료

- Calendar snapshot page가 `title`/`location` allowlist를 받을 수 있게 했다. `location`은 `title` 없이 받을 수 없고, 상세 필드가 있으면 `calendar.Deps.SecretBox`가 없을 때 요청을 실패시킨다.
- 상세 allowlist는 `calendar_snapshot_facts_staging`에 staging 전용 purpose로 암호화해 저장하고, snapshot complete transaction에서만 복호화한다.
- snapshot complete는 최신 `party_visibility_settings`를 다시 읽어 `details` 설정일 때만 projection을 details로 승격한다. `share_location=false`이면 location ciphertext를 쓰지 않는다.
- 최종 projection ciphertext purpose는 schedule read와 맞추기 위해 `party_projection_title:<projection UUID>`, `party_projection_location:<projection UUID>`로 고정했다.
- 본인 `GET/PATCH /v1/parties/{id}/visibility/me`, Party `GET /v1/parties/{id}/schedule`을 추가했다. schedule은 활성 멤버만 볼 수 있고 복호화 실패 시 해당 항목을 `busyOnly`로 강등한다.
- 공개 수준 하향 시 projection과 ciphertext를 같은 트랜잭션에서 지우고 활성 멤버에게 version이 있는 tombstone을 보낸다. 삭제 후 재생성에는 새 무작위 projection ID를 써 이전 tombstone version과 충돌하지 않는다.
- snapshot 세션의 `details_included`와 설정의 `details_ready_version`으로 상세 갱신 대기 상태를 설정 version에 묶었다. 상세 allowlist가 모두 들어온 snapshot 완료 뒤 `not_required`가 된다.
- 같은 상세 snapshot을 재실행해도 암호화 nonce 변화만으로 projection version이나 sync 변경이 늘지 않게 평문을 비교한다. 장소 공유를 끄면 제목은 유지하고 장소 ciphertext만 삭제한다.

## 검증 완료

- 로컬 PostgreSQL migration 00013~00015 적용 및 공개 수준·상세 snapshot 통합 테스트 통과. 상세 allowlist가 사라진 snapshot의 안전 강등도 검증했다.
- `REQUIRE_DB_TESTS=1 go test -p 1 ./... -count=1` 전체 통과(로컬 PostgreSQL). `go vet ./...`, OpenAPI YAML 파싱, `git diff --check` 통과.

## 남은 작업

- bootstrap의 Party scope 교체, 탈퇴·해산 cleanup, iOS 설정·캐시, privacy 경쟁 검증은 V3~V6에 남아 있다.
- OpenAPI 스키마와 실제 응답 자동 대조는 별도 공통 작업으로 남아 있다.
