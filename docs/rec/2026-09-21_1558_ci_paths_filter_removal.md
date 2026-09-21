# server-ci paths 필터 제거와 required check 지정 시도

- 날짜: 2026-09-21
- 브랜치: `feature/ci-required-check` → `main` fast-forward (`56b5a63`)
- 성격: 설계·구현 방향 확정 + 검증 완료 (required check 지정은 막힘)

## 결정: 필터를 없애고 항상 실행한다

`server-ci`를 required status check로 지정하려면, 워크플로 수준의 `paths` 필터 때문에
`docs/`만 바꾼 PR에는 check가 아예 보고되지 않아 병합이 pending에서 멈추는 문제를
먼저 풀어야 했다.

- **채택: push와 pull_request의 `paths` 필터를 모두 삭제한다.** check 이름이 늘
  보고되고, 초록불은 언제나 "테스트가 돌아서 통과"라는 뜻이 된다.
- **탈락: job 안에서 변경 경로를 보고 조건부로 skip하는 방식.** GitHub은 skip된 job을
  required check에서 성공으로 취급한다. 그러면 초록불이 다시 "통과"와 "안 돌림"을
  구분하지 못하는데, 이것이 M-8이 없애려던 모호함이다. 변경 감지 로직
  (`fetch-depth`, 새 브랜치의 zero `before`, PR merge ref)이 틀리면 스키마 불변식
  테스트가 조용히 빠진다는 위험도 있다. job은 1분 20초 정도라 절약할 비용이 작다.

## 검증

- 브랜치 push run `35570650323`: `success`, `--- PASS` 64, `--- SKIP` 0
- required check에 등록할 이름은 job 이름 `test`다. 워크플로 이름 `server-ci`가
  아니다. `gh api repos/hongsh26/plan_app/commits/ea6d095/check-runs`로 확인했다.
  `server-ci`를 등록하면 그 이름으로 보고되는 check가 없어 모든 PR이 pending으로 남는다.

## 막힌 것: required check 지정

`gh api repos/hongsh26/plan_app/branches/main/protection`과
`.../rulesets` 모두 `403 Upgrade to GitHub Pro or make this repository public to enable
this feature.`를 반환했다. 저장소는 개인 계정 소유의 private 저장소다.
branch protection과 ruleset 어느 쪽으로도 지정할 수 없다.

선택지는 사용자 결정 사항이다.

1. GitHub Pro로 업그레이드한 뒤 `main`에 required check `test`를 지정한다
2. 저장소를 public으로 바꾼다
3. 지정하지 않고, 병합 전에 CI 결과를 사람이 확인하는 관례로 운영한다

필터 제거는 이미 `main`에 들어갔으므로, 1이나 2를 고르면 check 이름 `test`를 등록하기만
하면 된다. 등록한 뒤에는 `docs/`만 바꾼 임시 PR을 열어
`gh pr view N --json mergeStateStatus`로 pending이 아닌지 확인한다.
