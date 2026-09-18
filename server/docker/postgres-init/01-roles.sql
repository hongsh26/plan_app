-- DB 역할 분리. account_backend_design.md §11: "DB 역할은 migration, api,
-- worker, read-only 운영 계정으로 분리한다."
--
-- 이 스크립트는 컨테이너 첫 기동 시 한 번만 실행된다. 비밀번호는 로컬 개발
-- 전용이며 docker-compose.yml에 평문으로 있다. 스테이징·프로덕션은 §12에 따라
-- 자격을 분리하고 secret manager에서 주입한다. 이 파일의 값을 쓰지 않는다.
--
-- PostgreSQL 15부터 schema public의 CREATE 권한이 PUBLIC에서 회수됐다.
-- migration 역할에 명시적으로 주지 않으면 마이그레이션이 테이블을 만들지
-- 못한다.

-- 마이그레이션 역할. DDL 권한을 가진 유일한 역할이다.
CREATE ROLE plantogether_migration LOGIN PASSWORD 'local_migration_password';

-- 런타임 역할. DDL 권한이 없다.
CREATE ROLE plantogether_api LOGIN PASSWORD 'local_api_password';
CREATE ROLE plantogether_worker LOGIN PASSWORD 'local_worker_password';

-- 운영 조회 전용 역할.
CREATE ROLE plantogether_readonly LOGIN PASSWORD 'local_readonly_password';

GRANT CONNECT ON DATABASE plantogether TO
    plantogether_migration, plantogether_api, plantogether_worker, plantogether_readonly;

GRANT USAGE ON SCHEMA public TO
    plantogether_api, plantogether_worker, plantogether_readonly;

-- migration 역할만 테이블을 만들 수 있다.
GRANT CREATE, USAGE ON SCHEMA public TO plantogether_migration;

-- migration 역할이 앞으로 만들 테이블에 런타임 역할의 권한을 미리 정해 둔다.
-- FOR ROLE을 명시하지 않으면 이 스크립트를 실행하는 superuser에게만 적용되어
-- 마이그레이션이 만든 테이블에는 걸리지 않는다.
ALTER DEFAULT PRIVILEGES FOR ROLE plantogether_migration IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO plantogether_api, plantogether_worker;

ALTER DEFAULT PRIVILEGES FOR ROLE plantogether_migration IN SCHEMA public
    GRANT SELECT ON TABLES TO plantogether_readonly;

ALTER DEFAULT PRIVILEGES FOR ROLE plantogether_migration IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO plantogether_api, plantogether_worker;

-- §11: Apple provider refresh token 복호화 권한은 api 역할이 갖지 않는다.
-- KMS 키 접근 정책으로 강제하며, 이 스크립트가 아니라 배포 단계에서 다룬다.
-- P0 로컬 환경에는 KMS가 없으므로 ciphertext 열만 만들어 두고 비워 둔다.
