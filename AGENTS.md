# Project overview

새로운 mcp 구현을 위한 프로젝트

# Project structure

project-root/
├── docs/               #프로젝트 문서
│   └── rec/            #작업 기록(Handoff)
├── runs/               #실행 결과
├── src/
│   ├── base/                   #기존 mcp
│   └── new/                    #새로운 mcp
└── .env                        #환경 변수

# Architecture rules

- 언제든 기존 코드를 돌릴 수 있어야한다.

# Working rules

- 궁금한 정보가 있다면 docs 폴더 내부를 탐색한다.
- 항상 작업을 시작하기전 /docs에 있는 progressing.md를 읽고 시작한다.
- 향후 진행해야할 작업과 진행중이던 작업을 중심으로 progressing.md에 업데이트한다.
- 코드 변경 후 관련 테스트를 실행한다.
- 작업 연속성을 위해 완료된 작업에 대해서는 /docs/rec에 Markdown문서로 기록한다.
    - 파일명은 시간, 분 단위도 포함시킨다.
    - 기능 구현 완료
    - 버그 수정 완료
    - 검증 완료
    - 리팩터링 완료
    - 설계 또는 구현 방향 확정
- 작업을 마치고 서브 에이전트가 코드 및 작성 문서에 대해 항상 재검증한다.
- 초기 MVP를 Main 브랜치에서 작업을 하고 이후 새로운 기능을 추가할 때는 기능 이름으로된 브랜치에서 작업을 진행하고 완성이되었다면 메인으로 병합한다.