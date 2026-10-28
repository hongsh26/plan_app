# 프로젝트 Goal·End 확정

## 설계 또는 구현 방향 확정

- 프로젝트가 끝없이 늘어나는 것을 막으려고 Goal과 End를 정했다. 문서는 `docs/project_goal.md`
- **Goal = Launch MVP**(`feature_design_backlog.md` 확정 설계 1의 필수 사용자 시나리오)
- **End = 체크리스트 15개(기능 10, 출시 조건 5)를 모두 충족하면 신규 기능 개발을 멈추고 종료한다.** 이후 작업은 새 Goal을 정할 때만 재개한다
- `CLAUDE.md` Working rules와 `progressing.md` 첫 줄에서 이 문서를 가리킨다

## 탈락한 대안

- Internal Alpha·시연 범위: 사용자가 Launch MVP를 골랐다. 시연은 중간 이정표로만 남긴다
- 기한·작업 단위 상한 추가: 사용자가 체크리스트 기준만 골랐다

## 체크리스트 구성에서 판단한 것

- 백로그의 Launch MVP 8개 항목을 기능 10개로 나눴다(로그인·동기화·캘린더 연결을 분리)
- progressing.md의 "잊으면 안 되는 것"(KMS, OpenAPI 자동 대조, P8 계정 삭제 연동)과 배포 설정을 출시 조건에 넣었다
- 15번(TestFlight, 개인정보 처리방침·라벨)은 백로그 Launch MVP 목록에는 없지만 출시 자체의 전제라 추가했다
- 백로그 Launch MVP 목록에는 "초대" 푸시가 있지만 설계 9(백로그 표 10행)에 따라 초대 푸시는 없다. 9번은 설계 9를 따른다
- 재검증(verifier): High 0, Medium 3을 반영했다. 판정 기준을 명시하고, 1번 "서버 완료" 과장을 바로잡고, 요청 제한 소비자를 12번에 넣었다. progressing.md의 "목표: 시연"은 중간 이정표로 고쳤다. Low 중 프로필, 오프라인 재시도, P10, 12·15번 증거 기준, CLAUDE.md 예외 문구도 반영했다
- 백로그 기능 12(접근성·분석 지표·오류 관측)는 상세 설계 11에서 우선순위를 다시 정하기로 돼 있어 넣지 않았다. 넣으려면 사용자 승인으로 체크리스트를 바꾼다

## 같은 날 함께 한 일

- `feature/party-visibility`의 미커밋 작업(공개 수준 서버 V1~V3, bootstrap scope, P5 cleanup)을 CI와 같은 절차로 검증하고 커밋했다(`aed8b93`). gofmt·vet 통과, `REQUIRE_DB_TESTS=1`로 전체 테스트 통과, skip 0건, 마이그레이션 reset→up 왕복 정상
