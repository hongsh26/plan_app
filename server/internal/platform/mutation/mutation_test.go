package mutation

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/platform/postgres/pgtest"
)

// 실제 PostgreSQL에서 api 런타임 역할로 helper의 계약을 검증한다.

type fixture struct {
	pool    *pgxpool.Pool
	user    uuid.UUID
	device  uuid.UUID
	jobType string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	ctx := context.Background()
	f := &fixture{pool: pool, user: uuid.New(), device: uuid.New(), jobType: "test." + uuid.NewString()}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, display_name) VALUES ($1, 'mutation 테스트')`, f.user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO devices (id, user_id, platform) VALUES ($1, $2, 'ios')`, f.device, f.user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// users 삭제가 devices, idempotency_keys, sync_changes를 cascade로 지운다.
		// outbox_jobs는 FK가 없으므로 테스트 전용 type으로 지운다.
		_, _ = pool.Exec(context.Background(), `DELETE FROM outbox_jobs WHERE type = $1`, f.jobType)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, f.user)
	})
	return f
}

func (f *fixture) req(key string, body string) Request {
	return Request{UserID: f.user, DeviceID: f.device, Key: key, Hash: RequestHash("PATCH", "PATCH /v1/me", []byte(body))}
}

// bump는 users.version을 올리고 sync 변경 하나를 남기는 도메인 mutation이다.
func (f *fixture) bump(calls *atomic.Int32) Func {
	return func(ctx context.Context, tx *Tx) (Result, error) {
		calls.Add(1)
		var v int64
		if err := tx.QueryRow(ctx,
			`UPDATE users SET version = version + 1 WHERE id = $1 RETURNING version`, f.user).Scan(&v); err != nil {
			return Result{}, err
		}
		if err := tx.EmitChange(ctx, Change{Recipient: f.user, EntityType: "user", EntityID: f.user, Version: &v,
			Payload: map[string]any{"v": v}}); err != nil {
			return Result{}, err
		}
		return Result{StatusCode: 200, ResourceType: "user", ResourceID: f.user, ResourceVersion: v}, nil
	}
}

func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) userVersion(t *testing.T) int64 {
	t.Helper()
	var v int64
	if err := f.pool.QueryRow(context.Background(), `SELECT version FROM users WHERE id = $1`, f.user).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// §7.4: 같은 키 재전송은 원래 결과를 돌려주고 다시 실행하지 않는다.
func TestRunReplaysSameKeyWithoutReexecuting(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	ctx := context.Background()

	first, err := Run(ctx, f.pool, f.req("k1", `{"a":1}`), time.Now(), f.bump(&calls))
	if err != nil || first.Replayed {
		t.Fatalf("첫 실행: %+v, %v", first, err)
	}
	second, err := Run(ctx, f.pool, f.req("k1", `{"a":1}`), time.Now(), f.bump(&calls))
	if err != nil {
		t.Fatalf("재전송: %v", err)
	}
	if !second.Replayed || second.Result != first.Result {
		t.Fatalf("재전송 결과 %+v, want 저장된 %+v", second, first.Result)
	}
	if calls.Load() != 1 || f.userVersion(t) != 2 {
		t.Fatalf("재전송이 다시 실행됐다: 실행 %d회, version %d", calls.Load(), f.userVersion(t))
	}
	if n := f.count(t, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1`, f.user); n != 1 {
		t.Errorf("sync 변경 %d개, want 1", n)
	}
}

// §8: 멱등성 키의 request hash가 다르면 원래 결과를 재사용하지 않는다.
func TestRunRejectsSameKeyWithDifferentRequest(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	ctx := context.Background()
	if _, err := Run(ctx, f.pool, f.req("k", `{"a":1}`), time.Now(), f.bump(&calls)); err != nil {
		t.Fatal(err)
	}
	_, err := Run(ctx, f.pool, f.req("k", `{"a":2}`), time.Now(), f.bump(&calls))
	if !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("err = %v, want ErrIdempotencyMismatch", err)
	}
	// 다른 endpoint에 같은 키와 같은 본문을 쓴 경우도 다른 요청이다.
	other := f.req("k", `{"a":1}`)
	other.Hash = RequestHash("DELETE", "DELETE /v1/devices/{id}", []byte(`{"a":1}`))
	if _, err := Run(ctx, f.pool, other, time.Now(), f.bump(&calls)); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("다른 endpoint err = %v, want ErrIdempotencyMismatch", err)
	}
	if calls.Load() != 1 {
		t.Errorf("mismatch인데 실행됐다: %d회", calls.Load())
	}
}

// §8 필수 불변식: 도메인 상태 전이와 sync_changes·outbox_jobs는 같은 트랜잭션에서
// 커밋한다. 모두 쓴 뒤 실패를 주입해 아무것도 남지 않는지 본다. 각 쓰기가 따로
// 커밋되는 구현이면 이 테스트가 실패한다.
func TestRunIsAtomicWhenDomainFails(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("주입된 실패")
	dedupe := "d-" + uuid.NewString()

	_, err := Run(context.Background(), f.pool, f.req("atomic", `{}`), time.Now(),
		func(ctx context.Context, tx *Tx) (Result, error) {
			if _, err := tx.Exec(ctx, `UPDATE users SET display_name = '바뀜', version = version + 1 WHERE id = $1`, f.user); err != nil {
				return Result{}, err
			}
			v := int64(2)
			if err := tx.EmitChange(ctx, Change{Recipient: f.user, EntityType: "user", EntityID: f.user, Version: &v, Payload: map[string]any{}}); err != nil {
				return Result{}, err
			}
			if err := tx.Enqueue(ctx, Job{Type: f.jobType, Payload: map[string]any{}, DedupeKey: &dedupe}); err != nil {
				return Result{}, err
			}
			return Result{}, boom
		})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want 주입된 실패", err)
	}

	if f.userVersion(t) != 1 {
		t.Error("도메인 변경이 남았다")
	}
	if n := f.count(t, `SELECT count(*) FROM sync_changes WHERE recipient_user_id = $1`, f.user); n != 0 {
		t.Errorf("sync 변경 %d개가 남았다", n)
	}
	if n := f.count(t, `SELECT count(*) FROM outbox_jobs WHERE type = $1`, f.jobType); n != 0 {
		t.Errorf("outbox job %d개가 남았다", n)
	}
	if n := f.count(t, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1`, f.user); n != 0 {
		t.Errorf("실패한 요청의 idempotency 기록 %d개가 남았다. 재시도가 막힌다", n)
	}

	// 실패는 기록되지 않으므로 같은 키로 재시도하면 실행된다.
	var calls atomic.Int32
	out, err := Run(context.Background(), f.pool, f.req("atomic", `{}`), time.Now(), f.bump(&calls))
	if err != nil || out.Replayed || calls.Load() != 1 {
		t.Fatalf("실패 후 재시도가 실행되지 않았다: %+v, %v", out, err)
	}
}

// 같은 키로 동시에 여러 요청이 와도 도메인 mutation은 정확히 한 번 실행되고,
// 나머지는 그 결과를 돌려받는다.
func TestRunConcurrentSameKeyExecutesOnce(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32

	const n = 8
	outs := make([]Outcome, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = Run(context.Background(), f.pool, f.req("race", `{}`), time.Now(), f.bump(&calls))
		}()
	}
	wg.Wait()

	replayed := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("동시 요청 %d 실패: %v", i, errs[i])
		}
		if outs[i].Result != outs[0].Result {
			t.Fatalf("동시 요청이 서로 다른 결과를 받았다: %+v, %+v", outs[0].Result, outs[i].Result)
		}
		if outs[i].Replayed {
			replayed++
		}
	}
	if calls.Load() != 1 || replayed != n-1 {
		t.Fatalf("실행 %d회, 재전송 %d건, want 1회와 %d건", calls.Load(), replayed, n-1)
	}
	if f.userVersion(t) != 2 {
		t.Errorf("version = %d, want 2", f.userVersion(t))
	}
}

// 만료된 기록은 없는 것과 같다(§8 기본 30일 보존).
func TestRunReexecutesAfterExpiry(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	now := time.Now()
	if _, err := Run(context.Background(), f.pool, f.req("old", `{}`), now, f.bump(&calls)); err != nil {
		t.Fatal(err)
	}
	later := now.Add(IdempotencyTTL + time.Minute)
	out, err := Run(context.Background(), f.pool, f.req("old", `{"다른":"본문"}`), later, f.bump(&calls))
	if err != nil || out.Replayed || calls.Load() != 2 {
		t.Fatalf("만료 후 재실행되지 않았다: %+v, %v, 실행 %d회", out, err, calls.Load())
	}
}

// 키는 (user, device, key) 단위다. 다른 기기의 같은 키는 별개 요청이다.
func TestRunKeysAreScopedPerDevice(t *testing.T) {
	f := newFixture(t)
	other := uuid.New()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO devices (id, user_id, platform) VALUES ($1, $2, 'ios')`, other, f.user); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	if _, err := Run(context.Background(), f.pool, f.req("shared", `{}`), time.Now(), f.bump(&calls)); err != nil {
		t.Fatal(err)
	}
	req := f.req("shared", `{}`)
	req.DeviceID = other
	out, err := Run(context.Background(), f.pool, req, time.Now(), f.bump(&calls))
	if err != nil || out.Replayed || calls.Load() != 2 {
		t.Fatalf("다른 기기의 같은 키가 재전송으로 처리됐다: %+v, %v", out, err)
	}
}

// ordinal은 트랜잭션 단위 counter다. 수신자가 달라도 0, 1, 2로 이어진다.
func TestEmitChangeOrdinalIsPerTransaction(t *testing.T) {
	f := newFixture(t)
	second := uuid.New()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO users (id, display_name) VALUES ($1, '두 번째')`, second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, second) })

	_, err := Run(context.Background(), f.pool, f.req("multi", `{}`), time.Now(),
		func(ctx context.Context, tx *Tx) (Result, error) {
			v := int64(1)
			for _, rcpt := range []uuid.UUID{f.user, second, f.user} {
				if err := tx.EmitChange(ctx, Change{Recipient: rcpt, EntityType: "user", EntityID: f.user, Version: &v, Payload: map[string]any{}}); err != nil {
					return Result{}, err
				}
			}
			// tombstone은 version이 없다.
			if err := tx.EmitChange(ctx, Change{Recipient: second, EntityType: "user", EntityID: second, Payload: map[string]any{}}); err != nil {
				return Result{}, err
			}
			return Result{StatusCode: 200, ResourceType: "user", ResourceID: f.user, ResourceVersion: 1}, nil
		})
	if err != nil {
		t.Fatalf("여러 수신자에게 쓰기 실패: %v", err)
	}

	rows, err := f.pool.Query(context.Background(), `
		SELECT ordinal, operation FROM sync_changes
		 WHERE recipient_user_id IN ($1, $2) ORDER BY txid, ordinal`, f.user, second)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ords []int32
	var ops []string
	for rows.Next() {
		var o int32
		var op string
		_ = rows.Scan(&o, &op)
		ords = append(ords, o)
		ops = append(ops, op)
	}
	if len(ords) != 4 || ords[0] != 0 || ords[1] != 1 || ords[2] != 2 || ords[3] != 3 {
		t.Fatalf("ordinal = %v, want [0 1 2 3]", ords)
	}
	if ops[3] != "tombstone" || ops[0] != "upsert" {
		t.Errorf("operation = %v", ops)
	}
}

// 같은 (type, dedupe_key)의 활성 job은 하나다.
func TestEnqueueDedupesActiveJobs(t *testing.T) {
	f := newFixture(t)
	dedupe := "same"
	var calls atomic.Int32
	for i, key := range []string{"j1", "j2"} {
		_, err := Run(context.Background(), f.pool, f.req(key, `{}`), time.Now(),
			func(ctx context.Context, tx *Tx) (Result, error) {
				calls.Add(1)
				if err := tx.Enqueue(ctx, Job{Type: f.jobType, Payload: map[string]any{"i": i}, DedupeKey: &dedupe}); err != nil {
					return Result{}, err
				}
				return Result{StatusCode: 202, ResourceType: "user", ResourceID: f.user, ResourceVersion: 1}, nil
			})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM outbox_jobs WHERE type = $1`, f.jobType); n != 1 {
		t.Fatalf("활성 job %d개, want 1", n)
	}
}

func TestRequestHashSeparatesParts(t *testing.T) {
	a := RequestHash("PATCH", "/v1/me", []byte("x"))
	b := RequestHash("PATCH", "/v1/mex", []byte(""))
	if string(a) == string(b) {
		t.Fatal("구분자 없이 이어 붙인 해시가 충돌했다")
	}
	if string(RequestHash("PATCH", "/v1/me", []byte("x"))) != string(a) {
		t.Fatal("같은 입력의 해시가 다르다")
	}
}

func TestCheckVersion(t *testing.T) {
	if err := CheckVersion(3, 3); err != nil {
		t.Fatal(err)
	}
	var vc *VersionConflictError
	if err := CheckVersion(2, 3); !errors.As(err, &vc) || vc.Current != 3 {
		t.Fatalf("err = %v, want VersionConflictError{3}", err)
	}
}
