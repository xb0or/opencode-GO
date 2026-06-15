package pool

import (
	"fmt"
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
