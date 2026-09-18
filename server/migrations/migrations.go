// Package migrations는 SQL 마이그레이션 파일을 바이너리에 embed한다.
//
// goose를 라이브러리로 쓰고 embed.FS를 넘기므로 배포 환경에 goose 바이너리를
// 따로 설치할 필요가 없다. 마이그레이션 파일이 바이너리와 함께 이동해 버전이
// 어긋나지 않는다.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
