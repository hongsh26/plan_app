# Codex 시작 경고 수정

## 문제

- Codex 시작 시 `oh-my-claudecode 5.3.0`의 SessionEnd hook timeout/async 호환 경고가 반복되었다.
- 같은 플러그인의 `t` MCP 서버가 `${CLAUDE_PLUGIN_ROOT}` 경로를 사용해 초기화 중 broken pipe로 실패했다.

## 원인

- Codex 전용 `oh-my-codex`와 함께 Claude 전용 `oh-my-claudecode@omc` 플러그인이 Codex 설정에서 활성화되어 있었다.
- Claude 전용 플러그인이 Codex에서 지원 방식이 다른 SessionEnd 속성과 Claude 전용 경로 변수를 등록했다.

## 변경

- `/Users/hongseunghyuk/.codex/config.toml`에서 `[plugins."oh-my-claudecode@omc"]`의 `enabled`를 `false`로 변경했다.
- Codex 전용 `[plugins."oh-my-codex@oh-my-codex-local"]` 설정은 유지했다.
- 원본 설정은 `/Users/hongseunghyuk/.codex/config.toml.bak-20260910-1419`에 백업했다.

## 검증

- `codex mcp list`: 성공, `t` MCP 서버가 더 이상 등록되지 않음.
- `codex --help`: 성공, SessionEnd timeout/async 경고가 출력되지 않음.
- 제한된 실행 샌드박스의 PATH alias 경고는 별개이며 이번에 보고된 플러그인 경고와 관련이 없다.
