#!/usr/bin/env bash
# 로컬 시연용 api 실행. 개발용 로그인을 켜고 고정 개발 키를 쓴다.
# 배포 환경에서는 쓰지 않는다(APP_ENV=local 전용). 사전 조건: docker compose up -d --wait,
# go run ./cmd/api -migrate up.
set -euo pipefail
cd "$(dirname "$0")/.."

export APP_ENV=local
export DATABASE_URL="${DATABASE_URL:-postgres://plantogether_api:local_api_password@localhost:5432/plantogether?sslmode=disable}"
# 개발용 로그인은 인증을 우회하므로 로컬 DB에서만 쓴다.
[[ "$DATABASE_URL" =~ @(localhost|127\.0\.0\.1)(:|/) ]] || { echo "로컬 DB 전용 스크립트다" >&2; exit 1; }

# 실행할 때마다 무작위 키를 만든다(저장소에 키를 두지 않는다). 재기동하면 기존 세션은 무효가 되어
# 다시 로그인해야 한다.
export ACCESS_TOKEN_SIGNING_KEY="$(openssl rand -base64 32)"
export TOKEN_ENCRYPTION_KEY="$(openssl rand -base64 32)"
export APPLE_CLIENT_ID="${APPLE_CLIENT_ID:-com.example.plantogether}"
export DEV_LOGIN_ENABLED=true
# 이름만 알면 누구나 로그인할 수 있으므로 기본은 이 컴퓨터에서만 받는다. 실기기에서 접속해야
# 하면 HTTP_ADDR=0.0.0.0:8080을 명시하고, 신뢰하는 네트워크에서만 쓴다.
export HTTP_ADDR="${HTTP_ADDR:-127.0.0.1:8080}"

exec go run ./cmd/api
