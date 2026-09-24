package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

func testStore(t *testing.T) *models.Store {
	t.Helper()
	url := os.Getenv("MLDOJO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set MLDOJO_TEST_DATABASE_URL to run the auth integration tests")
	}
	st, err := models.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestAllowlistEndsExistingSessions is the regression test for the gap this
// covers: the allowlist used to be consulted only at login, so removing
// someone left their 30-day session working.
func TestAllowlistEndsExistingSessions(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id := "test-user-" + time.Now().Format("20060102150405.000000")
	u, err := st.UpsertUser(ctx, v1.User{ID: id, Provider: "conductor", Phone: "+8610000000000"})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	t.Cleanup(func() { st.DB.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id) })

	cookie := "cookie-" + id
	if err := st.CreateSession(ctx, hash(cookie), u.ID, "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	onList := New(Config{BaseURL: "https://x", ClientID: "c", ClientSecret: "s",
		AppBaseURL: "https://y", Allow: []string{"+8610000000000"}}, st)
	if got := onList.User(ctx, cookie); got == nil {
		t.Fatal("a user on the allowlist was refused")
	}

	// Same live session, allowlist no longer contains them.
	offList := New(Config{BaseURL: "https://x", ClientID: "c", ClientSecret: "s",
		AppBaseURL: "https://y", Allow: []string{"+8619999999999"}}, st)
	if got := offList.User(ctx, cookie); got != nil {
		t.Error("a user taken off the allowlist still has a working session")
	}
	// And the session is gone, so putting them back does not resurrect it.
	if got := onList.User(ctx, cookie); got != nil {
		t.Error("the stale session was not deleted, only refused")
	}
}

// TestEmptyAllowlistRefusesEveryone guards the original vulnerability: an
// empty allow used to mean "let everyone in".
func TestEmptyAllowlistRefusesEveryone(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id := "test-empty-" + time.Now().Format("20060102150405.000000")
	u, err := st.UpsertUser(ctx, v1.User{ID: id, Provider: "conductor", Phone: "+8610000000001"})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	t.Cleanup(func() { st.DB.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id) })

	cookie := "cookie-" + id
	if err := st.CreateSession(ctx, hash(cookie), u.ID, "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	m := New(Config{BaseURL: "https://x", ClientID: "c", ClientSecret: "s", AppBaseURL: "https://y"}, st)
	if m.Enabled() {
		t.Error("SSO is enabled with an empty allowlist")
	}
	if got := m.User(ctx, cookie); got != nil {
		t.Error("an empty allowlist admitted a user")
	}
}
