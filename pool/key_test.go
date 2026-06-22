package pool

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/opencode-sw/gateway/config"
	"github.com/opencode-sw/gateway/store"
)

// setupTestDB initializes a fresh in-memory SQLite for each test.
var testDBCounter int

func setupTestDB(t *testing.T) {
	t.Helper()
	config.Load()
	testDBCounter++
	dsn := fmt.Sprintf("file:memdb%d?mode=memory&cache=shared", testDBCounter)
	if err := store.InitForTest(dsn); err != nil {
		t.Fatalf("init test db: %v", err)
	}
}

func TestPicker_WeightedDistribution(t *testing.T) {
	setupTestDB(t)

	// Create 3 keys with weights 1, 2, 3.
	for _, w := range []int{1, 2, 3} {
		k := &store.Key{Value: "key-" + string(rune('0'+w)), Group: "test", Enabled: true, Weight: w}
		if err := store.DB().Create(k).Error; err != nil {
			t.Fatalf("create key weight=%d: %v", w, err)
		}
	}

	p := NewPicker()
	counts := map[string]int{}
	totalPicks := 600

	for i := 0; i < totalPicks; i++ {
		k, err := p.Pick("test")
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		counts[k.Value]++
	}

	// weight 1:expect ~100, weight 2: expect ~200, weight 3: expect ~300
	key1 := counts["key-1"]
	key2 := counts["key-2"]
	key3 := counts["key-3"]

	t.Logf("picks: key-1=%d key-2=%d key-3=%d (total=%d)", key1, key2, key3, totalPicks)

	// Allow 20% tolerance.
	if key1 < 70 || key1 > 130 {
		t.Errorf("key-1 (weight=1): got %d, want ~100", key1)
	}
	if key2 < 150 || key2 > 250 {
		t.Errorf("key-2 (weight=2): got %d, want ~200", key2)
	}
	if key3 < 240 || key3 > 360 {
		t.Errorf("key-3 (weight=3): got %d, want ~300", key3)
	}
}

func TestPicker_CooldownSkipsKeys(t *testing.T) {
	setupTestDB(t)

	// Create 2 keys, put one in cooldown.
	k1 := &store.Key{Value: "active-key", Group: "cd", Enabled: true, Weight: 1}
	k2 := &store.Key{Value: "cooldown-key", Group: "cd", Enabled: true, Weight: 1}
	future := time.Now().Add(10 * time.Minute)
	k2.CooldownUntil = &future
	store.DB().Create(k1)
	store.DB().Select("*").Create(k2)

	p := NewPicker()
	for i := 0; i < 10; i++ {
		k, err := p.Pick("cd")
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if k.Value == "cooldown-key" {
			t.Errorf("pick %d: should skip cooled-down key, got %s", i, k.Value)
		}
	}
}

func TestPicker_AllKeysInCooldown(t *testing.T) {
	setupTestDB(t)

	future := time.Now().Add(10 * time.Minute)
	k := &store.Key{Value: "only-key", Group: "allcd", Enabled: true, Weight: 1, CooldownUntil: &future}
	store.DB().Select("*").Create(k)

	p := NewPicker()
	_, err := p.Pick("allcd")
	if err != ErrNoAvailableKey {
		t.Errorf("expected ErrNoAvailableKey, got %v", err)
	}
}

func TestPicker_NoKeysForGroup(t *testing.T) {
	setupTestDB(t)
	p := NewPicker()
	_, err := p.Pick("nonexistent")
	if err != ErrNoAvailableKey {
		t.Errorf("expected ErrNoAvailableKey, got %v", err)
	}
}

func TestPicker_DisabledKeysSkipped(t *testing.T) {
	setupTestDB(t)

	// Create both as enabled, then disable k1 via update (avoids GORM zero-value issue).
	k1 := &store.Key{Value: "disabled-key", Group: "dis", Enabled: true, Weight: 1}
	k2 := &store.Key{Value: "enabled-key", Group: "dis", Enabled: true, Weight: 1}
	store.DB().Create(k1)
	store.DB().Create(k2)
	store.DB().Model(k1).Update("enabled", false)

	p := NewPicker()
	for i := 0; i < 5; i++ {
		k, err := p.Pick("dis")
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if k.Value == "disabled-key" {
			t.Errorf("pick %d: should skip disabled key", i)
		}
	}
}

func TestMarkFailure_ExponentialBackoff(t *testing.T) {
	setupTestDB(t)

	k := &store.Key{Value: "fail-key", Group: "fb", Enabled: true, Weight: 1}
	store.DB().Create(k)

	p := NewPicker()

	// Fail 3 times (threshold).
	p.MarkFailure(k.ID)
	p.MarkFailure(k.ID)
	p.MarkFailure(k.ID)

	var updated store.Key
	store.DB().First(&updated, k.ID)
	if updated.FailCount != 3 {
		t.Errorf("fail_count: got %d, want 3", updated.FailCount)
	}
	if updated.CooldownUntil == nil {
		t.Fatal("cooldown_until should be set after 3 failures")
	}

	// Fail again — cooldown should double.
	p.MarkFailure(k.ID)
	store.DB().First(&updated, k.ID)
	if updated.FailCount != 4 {
		t.Errorf("fail_count: got %d, want 4", updated.FailCount)
	}
}

func TestMarkSuccess_ResetsCooldown(t *testing.T) {
	setupTestDB(t)

	future := time.Now().Add(5 * time.Minute)
	k := &store.Key{Value: "recover-key", Group: "rec", Enabled: true, Weight: 1, FailCount: 5, CooldownUntil: &future}
	store.DB().Select("*").Create(k)

	p := NewPicker()
	p.MarkSuccess(k.ID)

	var updated store.Key
	store.DB().First(&updated, k.ID)
	if updated.FailCount != 0 {
		t.Errorf("fail_count: got %d, want 0", updated.FailCount)
	}
	if updated.CooldownUntil != nil {
		t.Errorf("cooldown_until should be nil after success")
	}
}

func TestResetCooldown(t *testing.T) {
	setupTestDB(t)

	future := time.Now().Add(5 * time.Minute)
	k := &store.Key{Value: "reset-key", Group: "rst", Enabled: true, Weight: 1, FailCount: 10, CooldownUntil: &future}
	store.DB().Select("*").Create(k)

	p := NewPicker()
	p.ResetCooldown(k.ID)

	var updated store.Key
	store.DB().First(&updated, k.ID)
	if updated.FailCount != 0 || updated.CooldownUntil != nil {
		t.Errorf("reset failed: fail_count=%d, cooldown_until=%v", updated.FailCount, updated.CooldownUntil)
	}
}

func TestParse429QuotaBucket(t *testing.T) {
	snapshot := `{"quota":{"rolling":{"usagePercent":80,"resetInSec":120},"weekly":{"usagePercent":100,"resetInSec":3600},"monthly":{"usagePercent":40,"resetInSec":7200}}}`
	body := []byte(`{"error":{"message":"weekly quota exceeded"}}`)

	bucket, resetInSec, ok := parse429QuotaBucket(body, snapshot)
	if !ok {
		t.Fatal("expected quota bucket to be parsed")
	}
	if bucket != "weekly" || resetInSec != 3600 {
		t.Fatalf("bucket=%s reset=%d, want weekly/3600", bucket, resetInSec)
	}
}

func TestMarkFailureWithQuota429Monthly100(t *testing.T) {
	setupTestDB(t)
	snapshot := `{"quota":{"rolling":{"usagePercent":90,"resetInSec":120},"weekly":{"usagePercent":20,"resetInSec":3600},"monthly":{"usagePercent":100,"resetInSec":7200}}}`
	k := &store.Key{Value: "quota-monthly-key", Group: "quota", Enabled: true, Weight: 1, QuotaSnapshot: snapshot}
	store.DB().Create(k)

	p := NewPicker()
	before := time.Now()
	p.MarkFailureWithQuota(k.ID, http.StatusTooManyRequests, []byte(`{"error":{"message":"monthly quota exceeded"}}`), snapshot)

	var updated store.Key
	store.DB().First(&updated, k.ID)
	if updated.FailCount != 1 {
		t.Fatalf("fail_count=%d, want 1", updated.FailCount)
	}
	if updated.CooldownUntil == nil {
		t.Fatal("cooldown_until should be set")
	}
	got := updated.CooldownUntil.Sub(before)
	if got < 7190*time.Second || got > 7210*time.Second {
		t.Fatalf("cooldown=%s, want about 7200s", got)
	}
}

func TestMarkFailureWithQuota429RollingFallback(t *testing.T) {
	setupTestDB(t)
	snapshot := `{"quota":{"rolling":{"usagePercent":95,"resetInSec":120},"weekly":{"usagePercent":20,"resetInSec":3600},"monthly":{"usagePercent":40,"resetInSec":7200}}}`
	k := &store.Key{Value: "quota-rolling-key", Group: "quota", Enabled: true, Weight: 1, QuotaSnapshot: snapshot}
	store.DB().Create(k)

	p := NewPicker()
	before := time.Now()
	p.MarkFailureWithQuota(k.ID, http.StatusTooManyRequests, []byte(`{"error":{"message":"rate limit exceeded"}}`), snapshot)

	var updated store.Key
	store.DB().First(&updated, k.ID)
	if updated.CooldownUntil == nil {
		t.Fatal("cooldown_until should be set")
	}
	got := updated.CooldownUntil.Sub(before)
	if got < 110*time.Second || got > 130*time.Second {
		t.Fatalf("cooldown=%s, want about 120s", got)
	}
}

func TestMarkFailureWithQuotaNon429Fallback(t *testing.T) {
	setupTestDB(t)
	k := &store.Key{Value: "quota-non429-key", Group: "quota", Enabled: true, Weight: 1}
	store.DB().Create(k)

	p := NewPicker()
	p.MarkFailureWithQuota(k.ID, http.StatusBadGateway, nil, "")

	var updated store.Key
	store.DB().First(&updated, k.ID)
	if updated.FailCount != 1 {
		t.Fatalf("fail_count=%d, want 1", updated.FailCount)
	}
	if updated.CooldownUntil != nil {
		t.Fatal("cooldown_until should not be set before exponential threshold")
	}
}

func TestPicker_UsageCountIncremented(t *testing.T) {
	setupTestDB(t)

	k := &store.Key{Value: "usage-key", Group: "usage", Enabled: true, Weight: 1}
	store.DB().Create(k)

	p := NewPicker()
	for i := 0; i < 5; i++ {
		_, err := p.Pick("usage")
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
	}

	var updated store.Key
	store.DB().First(&updated, k.ID)
	if updated.UsageCount != 5 {
		t.Errorf("usage_count: got %d, want 5", updated.UsageCount)
	}
}

func TestPickAttemptsStartsWithWeightedChoice(t *testing.T) {
	setupTestDB(t)

	for _, value := range []string{"attempt-key-1", "attempt-key-2", "attempt-key-3"} {
		if err := store.DB().Create(&store.Key{
			Value:   value,
			Group:   "attempts",
			Enabled: true,
			Weight:  1,
		}).Error; err != nil {
			t.Fatalf("create key %s: %v", value, err)
		}
	}

	p := NewPicker()
	first, err := p.PickAttempts("attempts")
	if err != nil {
		t.Fatalf("first PickAttempts: %v", err)
	}
	second, err := p.PickAttempts("attempts")
	if err != nil {
		t.Fatalf("second PickAttempts: %v", err)
	}

	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("attempt list lengths = %d and %d, want 3", len(first), len(second))
	}
	if first[0].Value != "attempt-key-1" {
		t.Fatalf("first attempt starts with %q, want attempt-key-1", first[0].Value)
	}
	if second[0].Value != "attempt-key-2" {
		t.Fatalf("second attempt starts with %q, want attempt-key-2", second[0].Value)
	}
	if first[1].Value != "attempt-key-2" || first[2].Value != "attempt-key-3" {
		t.Fatalf("first fallback order = %#v", []string{first[0].Value, first[1].Value, first[2].Value})
	}
}
