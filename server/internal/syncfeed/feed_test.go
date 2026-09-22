package syncfeed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/account"
	"plantogether/server/internal/auth"
	"plantogether/server/internal/notification"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/postgres/pgtest"
	"plantogether/server/internal/platform/secretbox"
)

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{Epoch: Epoch{SystemID: 7687170298575188004, Timeline: 3}, TxID: 1 << 40, Ordinal: -1}
	got, err := DecodeCursor(c.Encode())
	if err != nil || got != c {
		t.Fatalf("왕복 결과 %+v, %v, want %+v", got, err, c)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "!!!", "MQ", Cursor{TxID: 1}.Encode()[:3],
		// 다른 버전, 필드 수, ordinal < -1, 세대 없음, 숫자 아님
		b64("2.9.1.1.0"), b64("1.9.1.1"), b64("1.9.1.1.-2"), b64("1.0.1.1.0"), b64("1.9.0.1.0"), b64("1.9.1.x.0")} {
		if _, err := DecodeCursor(s); !errors.Is(err, ErrBadCursor) {
			t.Errorf("%q: err = %v, want ErrBadCursor", s, err)
		}
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// ---- PostgreSQL ----

type fx struct {
	pool *pgxpool.Pool
	user uuid.UUID
}

func newFx(t *testing.T) *fx {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.APIRoleURLEnv)
	f := &fx{pool: pool, user: uuid.New()}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, display_name) VALUES ($1, 'sync 테스트')`, f.user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, f.user) })
	return f
}

// emit은 q(트랜잭션 또는 pool)로 변경 한 줄을 쓰고 entity_id를 돌려준다. 변경의
// 순서를 확인하려고 entity_id를 표식으로 쓴다.
func (f *fx) emit(t *testing.T, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := q.Exec(context.Background(), `
		INSERT INTO sync_changes (ordinal, recipient_user_id, entity_type, entity_id, operation, entity_version, payload)
		VALUES (0, $1, 'test', $2, 'upsert', 1, '{}')`, f.user, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// drain은 cursor에서 시작해 변경이 기대 개수만큼 모일 때까지 읽는다. 다른 테스트
// 패키지의 트랜잭션이 horizon을 잠시 붙잡을 수 있으므로 기다린다.
func (f *fx) drain(t *testing.T, from Cursor, want int) ([]uuid.UUID, Cursor) {
	t.Helper()
	var got []uuid.UUID
	cur := from
	deadline := time.Now().Add(10 * time.Second)
	for {
		page, err := Read(context.Background(), f.pool, f.user, cur, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Changes {
			got = append(got, c.EntityID)
		}
		cur = page.Next
		if len(got) >= want && !page.HasMore {
			return got, cur
		}
		if time.Now().After(deadline) {
			t.Fatalf("10초 안에 변경 %d개를 받지 못했다(받은 것 %d개)", want, len(got))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fx) watermark(t *testing.T) Cursor {
	t.Helper()
	s, err := Bootstrap(context.Background(), f.pool, f.user)
	if err != nil {
		t.Fatal(err)
	}
	return s.Watermark
}

// §15 순서 역전 회귀 테스트.
//
// 트랜잭션 A가 먼저 sync_changes에 쓰고 커밋하지 않은 채, B가 쓰고 커밋한다. 이때
// 읽으면 B도 전달되지 않아야 한다. A가 horizon을 붙잡고 있기 때문이다. B만 전달하고
// cursor를 B 뒤로 옮기면, 나중에 A가 커밋해도 A는 cursor 앞에 있어 영구히 유실된다.
// A 커밋 뒤에는 A, B가 순서대로 정확히 한 번 전달돼야 한다.
//
// A와 B는 각각 실제 별도 연결이다. savepoint로는 "진행 중인 다른 트랜잭션"을 만들 수 없다.
func TestReadWithholdsChangesBehindInFlightTransaction(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	start := f.watermark(t)

	connA, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Release()
	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	// BEGIN만으로는 txid가 배정되지 않는다. 쓰기가 txid를 배정한다.
	idA := f.emit(t, txA)

	idB := f.emit(t, f.pool) // B: 자동 커밋

	page, err := Read(ctx, f.pool, f.user, start, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Changes {
		if c.EntityID == idB {
			t.Fatal("진행 중인 트랜잭션 A보다 뒤에 커밋된 B가 먼저 전달됐다. A가 커밋되면 cursor 앞에 떨어져 유실된다")
		}
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, end := f.drain(t, page.Next, 2)
	if len(got) != 2 || got[0] != idA || got[1] != idB {
		t.Fatalf("A 커밋 뒤 전달 = %v, want [A B] 순서로 한 번씩 (A=%s B=%s)", got, idA, idB)
	}

	// 다시 읽어도 같은 변경이 오지 않는다.
	again, err := Read(ctx, f.pool, f.user, end, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Changes) != 0 {
		t.Fatalf("이미 전달한 변경이 다시 왔다: %d개", len(again.Changes))
	}
}

func TestReadPaginatesWithoutLossOrDuplicates(t *testing.T) {
	f := newFx(t)
	start := f.watermark(t)
	var want []uuid.UUID
	// 한 트랜잭션에 여러 변경을 넣어 페이지 경계가 트랜잭션 안에서 갈라지게 한다.
	err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		for i := range 5 {
			id := uuid.New()
			if _, err := tx.Exec(context.Background(), `
				INSERT INTO sync_changes (ordinal, recipient_user_id, entity_type, entity_id, operation, entity_version, payload)
				VALUES ($1, $2, 'test', $3, 'upsert', 1, '{}')`, i, f.user, id); err != nil {
				return err
			}
			want = append(want, id)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var got []uuid.UUID
	cur := start
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < 5 {
		page, err := Read(context.Background(), f.pool, f.user, cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Changes) > 2 {
			t.Fatalf("limit 2인데 %d개를 돌려줬다", len(page.Changes))
		}
		for _, c := range page.Changes {
			got = append(got, c.EntityID)
		}
		cur = page.Next
		if time.Now().After(deadline) {
			t.Fatal("시간 안에 모두 받지 못했다")
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("페이지를 이어 받은 순서 %v, want %v", got, want)
		}
	}
}

// 정리 워터마크 이하의 cursor는 410이다. 그 뒤의 변경 일부가 이미 지워졌을 수
// 있으므로 이어 받으면 무언가 빠졌다는 사실조차 모르게 된다. 워터마크 뒤의 cursor는
// 영향이 없다.
func TestReadRejectsCursorAtOrBeforePruneWatermark(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	before := f.watermark(t)
	old := f.emit(t, f.pool)
	if _, err := f.pool.Exec(ctx,
		`UPDATE sync_changes SET created_at = now() - interval '40 days' WHERE entity_id = $1`, old); err != nil {
		t.Fatal(err)
	}
	// 정리 후 새 변경. 이것은 지워지지 않는다.
	var afterOld Cursor
	got, afterOld := f.drain(t, before, 1)
	if len(got) != 1 || got[0] != old {
		t.Fatalf("정리 전 전달 = %v", got)
	}
	fresh := f.emit(t, f.pool)

	n, err := Prune(ctx, f.pool, time.Now().Add(-Retention))
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("정리가 지운 개수 %d, want >= 1", n)
	}

	if _, err := Read(ctx, f.pool, f.user, before, 0); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("정리된 변경 앞의 cursor err = %v, want ErrCursorExpired", err)
	}
	got, _ = f.drain(t, afterOld, 1)
	if len(got) != 1 || got[0] != fresh {
		t.Fatalf("정리 워터마크 뒤의 cursor가 새 변경을 받지 못했다: %v", got)
	}

	// 지울 것이 없는 정리는 워터마크를 내리지 않는다.
	if _, err := Prune(ctx, f.pool, time.Now().Add(-Retention)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(ctx, f.pool, f.user, before, 0); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("두 번째 정리 뒤 워터마크가 내려갔다: err = %v", err)
	}
}

// 다른 클러스터 세대에서 발급한 cursor는 410이다. 장애 전환이나 시점 복구로 txid
// 이력이 되감기면, 옛 cursor 뒤에는 새 변경이 오지 않는다.
func TestReadRejectsCursorFromAnotherEpoch(t *testing.T) {
	f := newFx(t)
	c := f.watermark(t)
	for _, other := range []Epoch{
		{SystemID: c.Epoch.SystemID, Timeline: c.Epoch.Timeline + 1},
		{SystemID: c.Epoch.SystemID + 1, Timeline: c.Epoch.Timeline},
	} {
		stale := c
		stale.Epoch = other
		if _, err := Read(context.Background(), f.pool, f.user, stale, 0); !errors.Is(err, ErrCursorExpired) {
			t.Errorf("세대 %+v의 cursor err = %v, want ErrCursorExpired", other, err)
		}
	}
	if _, err := Read(context.Background(), f.pool, f.user, c, 0); err != nil {
		t.Fatalf("같은 세대의 cursor를 거부했다: %v", err)
	}
}

// bootstrap 시점에 진행 중이던 트랜잭션의 변경은 snapshot에 없고, 이후 증분으로
// 빠짐없이 와야 한다. watermark를 horizon이 아니라 더 뒤(xmax 등)로 두거나 horizon을
// 다른 트랜잭션에서 구하면 이 변경을 잃는다.
func TestBootstrapWatermarkKeepsInFlightChanges(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	connA, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Release()
	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	idA := f.emit(t, txA)

	wm := f.watermark(t)
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.drain(t, wm, 1)
	found := false
	for _, id := range got {
		found = found || id == idA
	}
	if !found {
		t.Fatalf("bootstrap 중 진행 중이던 변경 %s가 전달되지 않았다: %v", idA, got)
	}
}

// 다른 사용자의 변경은 전달되지 않는다. recipient_user_id가 권한의 전부다.
func TestReadReturnsOnlyOwnChanges(t *testing.T) {
	me := newFx(t)
	other := newFx(t)
	start := me.watermark(t)
	other.emit(t, other.pool)
	mine := me.emit(t, me.pool)

	got, _ := me.drain(t, start, 1)
	if len(got) != 1 || got[0] != mine {
		t.Fatalf("전달된 변경 %v, want 내 것 하나 %s", got, mine)
	}
}

// 변경이 없을 때도 cursor는 뒤로 가지 않는다.
func TestReadNeverMovesCursorBackward(t *testing.T) {
	f := newFx(t)
	start := f.watermark(t)
	page, err := Read(context.Background(), f.pool, f.user, start, 0)
	if err != nil {
		t.Fatal(err)
	}
	if start.after(page.Next) {
		t.Fatalf("cursor가 뒤로 갔다: %+v → %+v", start, page.Next)
	}
	if page.Next.Epoch != start.Epoch {
		t.Errorf("세대가 바뀌었다: %+v → %+v", start.Epoch, page.Next.Epoch)
	}
}

// ---- HTTP ----

func newHandler(t *testing.T, f *fx) http.Handler {
	t.Helper()
	box, err := secretbox.NewLocal("local", bytes.Repeat([]byte{5}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	mux := http.NewServeMux()
	passthrough := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: f.user})))
		})
	}
	NewHandler(Deps{Pool: f.pool, Logger: logger, RefKeyBox: box}).Register(mux, passthrough)
	return httpapi.Wrap(logger, mux)
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHTTPBootstrapThenSync(t *testing.T) {
	f := newFx(t)
	h := newHandler(t, f)

	rec := get(h, "/v1/sync/bootstrap")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status %d: %s", rec.Code, rec.Body.String())
	}
	var boot bootstrapResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &boot)
	if boot.SchemaVersion != SchemaVersion || len(boot.Snapshot) != 1 || boot.Snapshot[0].EntityType != "user" {
		t.Fatalf("bootstrap = %+v", boot)
	}
	if len(boot.NotificationRefKey) != 43 { // 32바이트 base64url
		t.Errorf("notification_ref_key 길이 %d", len(boot.NotificationRefKey))
	}

	id := f.emit(t, f.pool)
	var changes []Change
	cursor := boot.CursorWatermark
	deadline := time.Now().Add(10 * time.Second)
	for len(changes) == 0 && time.Now().Before(deadline) {
		rec = get(h, "/v1/sync?cursor="+cursor)
		if rec.Code != http.StatusOK {
			t.Fatalf("sync status %d: %s", rec.Code, rec.Body.String())
		}
		var page readResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		changes, cursor = page.Changes, page.Cursor
	}
	if len(changes) != 1 || changes[0].EntityID != id {
		t.Fatalf("bootstrap 뒤의 변경이 전달되지 않았다: %+v", changes)
	}

	// 같은 키를 다시 받는다.
	rec = get(h, "/v1/sync/bootstrap")
	var again bootstrapResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if again.NotificationRefKey != boot.NotificationRefKey {
		t.Error("bootstrap마다 notification_ref_key가 바뀐다")
	}
}

func TestHTTPSyncErrors(t *testing.T) {
	f := newFx(t)
	h := newHandler(t, f)
	wm := f.watermark(t)
	stale := wm
	stale.Epoch.Timeline++
	expired := stale.Encode()
	for _, tc := range []struct {
		path   string
		status int
		code   string
	}{
		{"/v1/sync", 400, httpapi.CodeInvalidRequest},
		{"/v1/sync?cursor=garbage", 400, httpapi.CodeInvalidRequest},
		{"/v1/sync?cursor=" + wm.Encode() + "&limit=0", 400, httpapi.CodeInvalidRequest},
		{"/v1/sync?cursor=" + expired, 410, httpapi.CodeSyncCursorExpired},
	} {
		rec := get(h, tc.path)
		var e httpapi.ErrorResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != tc.status || e.Code != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.path, rec.Code, e.Code, tc.status, tc.code)
		}
	}
}

// 처음 받는 요청이 동시에 여러 번 와도 키는 하나다. 기기마다 다른 키를 받으면 한쪽의
// 알림 역조회가 영원히 실패한다.
func TestEnsureRefKeyConcurrentFirstUse(t *testing.T) {
	f := newFx(t)
	box, _ := secretbox.NewLocal("local", bytes.Repeat([]byte{5}, secretbox.KeySize))
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = notification.EnsureRefKey(context.Background(), f.pool, box, f.user)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("요청 %d 실패: %v", i, errs[i])
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Fatal("동시 첫 요청이 서로 다른 키를 받았다")
		}
	}
	var stored []byte
	_ = f.pool.QueryRow(context.Background(),
		`SELECT ref_key_ciphertext FROM notification_ref_keys WHERE user_id = $1`, f.user).Scan(&stored)
	if bytes.Contains(stored, keys[0]) {
		t.Fatal("참조 키가 평문으로 저장됐다")
	}
}

// 증분 변경은 bootstrap과 같은 전체 투영을 싣는다. 일부 필드만 실으면 클라이언트는
// 병합 규칙을 알아야 하고, 로컬에 없는 entity의 upsert를 채울 수 없다.
func TestIncrementalPayloadIsFullProjection(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	boot, err := Bootstrap(ctx, f.pool, f.user)
	if err != nil {
		t.Fatal(err)
	}
	var snapUser map[string]any
	_ = json.Unmarshal(boot.Entities[0].Payload, &snapUser)

	// 표시 이름만 바꾸는 mutation과 같은 경로로 변경을 남긴다.
	err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET display_name = '바뀐 이름', version = version + 1 WHERE id = $1`, f.user); err != nil {
			return err
		}
		pr, err := account.UserProjection(ctx, tx, f.user)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(pr.Payload)
		_, err = tx.Exec(ctx, `
			INSERT INTO sync_changes (ordinal, recipient_user_id, entity_type, entity_id, operation, entity_version, payload)
			VALUES (0, $1, 'user', $1, 'upsert', $2, $3)`, f.user, pr.Version, b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := Read(ctx, f.pool, f.user, boot.Watermark, 0)
	if err != nil || len(page.Changes) == 0 {
		t.Fatalf("변경을 받지 못했다: %v", err)
	}
	var inc map[string]any
	_ = json.Unmarshal(page.Changes[len(page.Changes)-1].Payload, &inc)
	for k := range snapUser {
		if _, ok := inc[k]; !ok {
			t.Errorf("증분 payload에 snapshot 필드 %q가 없다: %v", k, inc)
		}
	}
}

// 정리 워터마크는 내려가지 않는다. created_at 순서와 txid 순서가 어긋나므로, 나중
// 정리가 워터마크보다 앞(작은 txid)의 row를 지울 수 있다. 그때 워터마크를 그 위치로
// 덮으면 이미 410이어야 할 cursor가 되살아나 첫 정리에서 지운 row를 건너뛴다.
func TestPruneWatermarkNeverMovesBackward(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	older := f.emit(t, f.pool) // B: 작은 txid
	newer := f.emit(t, f.pool) // A: 큰 txid

	var txNewer string
	if err := f.pool.QueryRow(ctx, `SELECT txid::text FROM sync_changes WHERE entity_id = $1`, newer).Scan(&txNewer); err != nil {
		t.Fatal(err)
	}
	backdate := func(id uuid.UUID) {
		if _, err := f.pool.Exec(ctx,
			`UPDATE sync_changes SET created_at = now() - interval '40 days' WHERE entity_id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	backdate(newer)
	if _, err := Prune(ctx, f.pool, time.Now().Add(-Retention)); err != nil {
		t.Fatal(err)
	}
	backdate(older)
	if _, err := Prune(ctx, f.pool, time.Now().Add(-Retention)); err != nil {
		t.Fatal(err)
	}

	// B보다 뒤, A 이하의 위치. 워터마크가 A에 머물렀으면 410이다. B로 내려갔으면 통과해
	// 첫 정리에서 지운 A를 조용히 건너뛴다.
	txid, err := strconv.ParseUint(txNewer, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	between := f.watermark(t)
	between.TxID, between.Ordinal = txid, -1
	if _, err := Read(ctx, f.pool, f.user, between, 0); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("워터마크가 내려갔다: A 앞의 cursor err = %v, want ErrCursorExpired", err)
	}
}

// 운영에서 Prune은 scheduler가 worker 역할로 돌린다. api 역할로만 테스트하면 worker
// 역할에 빠진 권한이 운영에서야 드러난다. batch를 여러 번 나눠 돌아도 끝에는 지운 row가
// 모두 워터마크 이하다.
func TestPruneAsWorkerRoleAcrossBatches(t *testing.T) {
	f := newFx(t)
	worker := pgtest.Pool(t, pgtest.WorkerRoleURLEnv)
	ctx := context.Background()

	ids := []uuid.UUID{f.emit(t, f.pool), f.emit(t, f.pool), f.emit(t, f.pool)}
	if _, err := f.pool.Exec(ctx,
		`UPDATE sync_changes SET created_at = now() - interval '40 days' WHERE entity_id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
	var ourMax string
	if err := f.pool.QueryRow(ctx,
		`SELECT max(txid)::text FROM sync_changes WHERE entity_id = ANY($1)`, ids).Scan(&ourMax); err != nil {
		t.Fatal(err)
	}

	// 테스트 전용 row만 지운다는 보장이 없으므로(공유 DB) 우리 row가 모두 사라질 때까지
	// batch 1개씩 돌린다.
	for i := 0; ; i++ {
		if i > 1000 {
			t.Fatal("정리가 끝나지 않는다")
		}
		n, err := pruneOnce(ctx, worker, time.Now().Add(-Retention), 1)
		if err != nil {
			t.Fatalf("worker 역할의 정리 실패: %v", err)
		}
		var gone []string
		rows, _ := f.pool.Query(ctx, `
			SELECT t.id::text FROM unnest($1::uuid[]) t(id)
			 WHERE NOT EXISTS (SELECT 1 FROM sync_changes c WHERE c.entity_id = t.id)`, ids)
		gone, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if len(gone) == len(ids) || n == 0 {
			if len(gone) != len(ids) {
				t.Fatalf("정리가 끝났는데 row가 남았다: 지워진 것 %v", gone)
			}
			break
		}
	}

	// 지운 우리 row 중 가장 뒤의 것도 워터마크 이하다. 그 위치 앞의 cursor는 410이다.
	var covered bool
	if err := f.pool.QueryRow(ctx,
		`SELECT pruned_through_txid >= $1::xid8 FROM sync_prune_state`, ourMax).Scan(&covered); err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatal("지운 row가 워터마크 뒤에 있다")
	}
	txid, err := strconv.ParseUint(ourMax, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	c := f.watermark(t)
	c.TxID, c.Ordinal = txid, -1
	if _, err := Read(ctx, f.pool, f.user, c, 0); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("워터마크 이하 cursor err = %v, want ErrCursorExpired", err)
	}
}
