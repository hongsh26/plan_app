# Party 이름 길이 계약: rune 기준 확정

## 결정

Party 이름 길이 상한 40자는 **Unicode code point(rune) 기준**으로 센다. 설계 §4.1의 "grapheme cluster 기준"을 개정했다.

## 근거와 탈락한 대안

- 탈락: grapheme cluster 기준. Unicode 분절 라이브러리라는 새 의존성이 필요하고, 저장소 규칙상 새 의존성은 명시적 요청이 필요하다.
- 감수하는 차이: 결합 이모지(가족, 피부색 조합 등)는 여러 rune으로 세므로 사용자가 보기에는 40자 미만이어도 거부될 수 있다. NFC 정규화 후 세므로 한글 분해형은 조합형과 같게 센다.
- DB에는 길이 제약이 없고 애플리케이션(`NormalizeName`)만 검증한다. 나중에 기준을 바꿔도 마이그레이션이 필요 없다.

## 변경

- `docs/party_membership_design.md` §4.1 길이 행
- `server/internal/party/names.go`: P2 구현에 포함된 미커밋 파일이다. 코드는 처음부터 rune 기준이었고, 이번에 "설계와 다르다"는 주석을 확정된 계약 설명으로 바꿨다
- `server/internal/party/names_test.go` 신규: 40/41 경계(ASCII, 한글, 분해형 한글), 공백 축약, 탭·NUL·BiDi override 거부(결합 이모지를 여러 rune으로 세는 케이스는 아직 없다)

## 검증

`go vet`, `gofmt`, `go test ./...` 통과. 탭은 제어문자라 거부되는 것이 설계 §4.1 금지 항목과 일치한다.
