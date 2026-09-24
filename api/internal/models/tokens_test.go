package models

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAPITokenLifecycle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	plain, meta, err := st.CreateAPIToken(ctx, "ci-"+t.Name(), "admin", "tester", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { st.DB.Exec(context.Background(), `DELETE FROM api_tokens WHERE id=$1`, meta.ID) })
	if !strings.HasPrefix(plain, "mld_") {
		t.Errorf("token %q does not look like an mldojo token", plain)
	}

	// The plaintext must not be recoverable from the database.
	var stored string
	if err := st.DB.QueryRow(ctx, `SELECT token_hash FROM api_tokens WHERE id=$1`, meta.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == plain || strings.Contains(stored, plain) {
		t.Error("the plaintext token is recoverable from the database")
	}

	got, err := st.AuthAPIToken(ctx, plain)
	if err != nil {
		t.Fatalf("auth with a fresh token: %v", err)
	}
	if got.ID != meta.ID || got.Role != "admin" {
		t.Errorf("auth returned %+v, want id %s role admin", got, meta.ID)
	}
	if got.LastUsedAt == nil {
		t.Error("last_used_at was not refreshed")
	}
	if _, err := st.AuthAPIToken(ctx, plain+"x"); err == nil {
		t.Error("a wrong token authenticated")
	}

	// Revoking by id prefix, the way run ids work.
	full, err := st.RevokeAPIToken(ctx, meta.ID[:8])
	if err != nil {
		t.Fatalf("revoke by prefix: %v", err)
	}
	if full != meta.ID {
		t.Errorf("revoke returned %s, want %s", full, meta.ID)
	}
	if _, err := st.AuthAPIToken(ctx, plain); err == nil {
		t.Error("a revoked token still authenticates")
	}
}

func TestAPITokenExpiry(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	plain, meta, err := st.CreateAPIToken(ctx, "short-"+t.Name(), "member", "tester", time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { st.DB.Exec(context.Background(), `DELETE FROM api_tokens WHERE id=$1`, meta.ID) })
	if meta.ExpiresAt == nil {
		t.Fatal("expires_at was not set")
	}
	if _, err := st.AuthAPIToken(ctx, plain); err != nil {
		t.Fatalf("a token an hour from expiry should work: %v", err)
	}
	// Move the expiry into the past rather than sleeping.
	if _, err := st.DB.Exec(ctx, `UPDATE api_tokens SET expires_at = now() - interval '1 minute' WHERE id=$1`, meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthAPIToken(ctx, plain); err == nil {
		t.Error("an expired token still authenticates")
	}
}
