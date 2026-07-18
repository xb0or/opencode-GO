package store

import (
	"testing"
)

// TestP1_1_TryReserveRequestDBError verifies that TryReserveRequest returns
// (false, err) — not (false, nil) — when the database is unavailable, so the
// middleware can distinguish "DB broken" (→ 503) from "quota exhausted" (→ 403).
func TestP1_1_TryReserveRequestDBError(t *testing.T) {
	if err := InitForTest("file:p11_store_dberr?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}

	// Create a token with MaxRequests > 0 so TryReserveRequest hits the DB.
	tok := Token{
		Token:       "test-dberr-token",
		Name:        "test-dberr",
		Enabled:     true,
		MaxRequests: 10,
	}
	if err := DB().Create(&tok).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}

	// Close the underlying SQL connection so the next DB op fails.
	sqlDB, err := DB().DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}
	_ = sqlDB.Close()

	reserved, err := TryReserveRequest(tok.ID)
	if reserved {
		t.Error("expected reserved=false when DB is closed, got true")
	}
	if err == nil {
		t.Error("expected a non-nil error when DB is closed, got nil — " +
			"middleware cannot distinguish DB error from quota exhaustion")
	}
}

// TestP1_1_TryReserveRequestUnlimitedNoWrite verifies that a token with
// MaxRequests <= 0 returns (true, nil) without any DB write, so unlimited
// tokens don't generate a write transaction per request.
func TestP1_1_TryReserveRequestUnlimitedNoWrite(t *testing.T) {
	if err := InitForTest("file:p11_store_unlimited?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}

	tok := Token{
		Token:       "test-unlimited-token",
		Name:        "test-unlimited",
		Enabled:     true,
		MaxRequests: 0, // unlimited
	}
	if err := DB().Create(&tok).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}

	for i := 0; i < 5; i++ {
		reserved, err := TryReserveRequest(tok.ID)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if !reserved {
			t.Fatalf("call %d: unlimited token should always be reserved", i)
		}
	}

	// requests_used must remain 0 — no DB writes for unlimited tokens.
	var refreshed Token
	if err := DB().First(&refreshed, tok.ID).Error; err != nil {
		t.Fatalf("reload token: %v", err)
	}
	if refreshed.RequestsUsed != 0 {
		t.Errorf("unlimited token requests_used = %d, want 0", refreshed.RequestsUsed)
	}
}

// TestP1_1_TryReserveRequestQuotaExhausted verifies that once requests_used
// reaches MaxRequests, TryReserveRequest returns (false, nil) — quota
// exhausted, not a DB error.
func TestP1_1_TryReserveRequestQuotaExhausted(t *testing.T) {
	if err := InitForTest("file:p11_store_quota?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}

	tok := Token{
		Token:       "test-quota-token",
		Name:        "test-quota",
		Enabled:     true,
		MaxRequests: 2,
	}
	if err := DB().Create(&tok).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}

	// First two requests succeed.
	for i := 0; i < 2; i++ {
		reserved, err := TryReserveRequest(tok.ID)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if !reserved {
			t.Fatalf("call %d: expected reserved=true (under cap)", i)
		}
	}

	// Third request must be rejected with (false, nil) — quota exhausted.
	reserved, err := TryReserveRequest(tok.ID)
	if err != nil {
		t.Fatalf("quota exhaustion must return nil error, got: %v", err)
	}
	if reserved {
		t.Error("expected reserved=false when quota exhausted, got true")
	}

	// requests_used must be exactly 2 (at the cap).
	var refreshed Token
	if err := DB().First(&refreshed, tok.ID).Error; err != nil {
		t.Fatalf("reload token: %v", err)
	}
	if refreshed.RequestsUsed != 2 {
		t.Errorf("requests_used = %d, want 2", refreshed.RequestsUsed)
	}
}