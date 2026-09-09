# PlanTogether iOS

SwiftUI와 EventKit으로 만든 그룹 일정 앱의 첫 실행 단위다. 현재는 메모리 기반 데모 Party, 공통 가능 시간 계산, 약속 제안, Apple 캘린더 읽기를 포함한다.

## 실행

1. macOS에 Xcode 15 이상과 XcodeGen을 설치한다.
2. 이 디렉터리에서 `xcodegen generate`를 실행한다.
3. 생성된 `PlanTogether.xcodeproj`를 열고 개발 팀과 번들 ID를 설정한다.
4. iOS 17 이상 시뮬레이터 또는 기기에서 실행한다.
5. `Product > Test`로 단위 테스트를 실행한다.

캘린더 권한과 실제 일정 확인은 기기 또는 일정 데이터가 들어 있는 시뮬레이터에서 검증한다. 현재 데이터는 앱 재시작 시 초기화된다.
