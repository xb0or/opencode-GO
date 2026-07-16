package pool

import (
	"log"
	"sync"
	"time"

	"github.com/xb0or/opencode-GO/store"
)

// QuotaCheckResult holds the outcome of checking one key's quota.
type QuotaCheckResult struct {
	KeyID   uint
	KeyValue string // masked by caller if needed
	Group   string
	// Expired is true when the key's credential (cookie/API key) is no longer
	// valid — the key should be disabled so the pool stops routing to it.
	Expired bool
	// Error carries any non-fatal error from the check (network timeout, parse
	// failure, etc.). The key is NOT disabled for transient errors.
	Error error
}

// QuotaChecker is a callback that checks a single key's credential validity.
// It returns expired=true when the credential is definitively invalid (HTTP
// 401/403, redirect-to-login, etc.), or an error for transient failures.
//
// The callback receives the full store.Key so it can read Cookie/WorkspaceID
// (Go group) or Value (Ollama API key group) as appropriate.
type QuotaChecker func(k store.Key) (expired bool, err error)

// CheckGroupQuotas checks quota validity for every enabled key in a group,
// running checks concurrently with a bounded worker pool. Keys whose
// credentials are definitively expired are disabled in the database so the
// Picker stops routing traffic to them.
//
// maxConcurrency caps the number of parallel HTTP checks (Ollama web-scrape
// checks are ~10x slower than Go API checks, so unbounded parallelism with
// 50+ keys would overwhelm the upstream or trip rate limits).
//
// This function is designed to be called from a background ticker/cron. It
// returns the aggregated results for observability/logging.
func CheckGroupQuotas(p *Picker, group string, check QuotaChecker, maxConcurrency int) []QuotaCheckResult {
	if maxConcurrency <= 0 {
		maxConcurrency = 8
	}

	// Load all enabled keys for the group directly from the store.
	var keys []store.Key
	if err := store.DB().Where("enabled = ? AND `group` = ?", true, group).
		Order("id asc").Find(&keys).Error; err != nil {
		log.Printf("quota check: failed to load keys for group %s: %v", group, err)
		return nil
	}
	if len(keys) == 0 {
		return nil
	}

	results := make([]QuotaCheckResult, len(keys))

	// Bounded concurrency: a semaphore channel limits parallel checks.
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	// Serialize DB writes (disable key) to avoid write contention — the read
	// side (PickAttempts) uses GORM which handles concurrent reads fine.
	var dbMu sync.Mutex

	for i, k := range keys {
		wg.Add(1)
		go func(idx int, key store.Key) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			expired, err := check(key)
			results[idx] = QuotaCheckResult{
				KeyID:    key.ID,
				KeyValue: key.Value,
				Group:    key.Group,
				Expired:  expired,
				Error:    err,
			}

			if expired {
				// Credential is definitively invalid — disable the key in the
				// database. Because the Picker reads keys from the store on
				// every PickAttempts call, disabling here is sufficient: the
				// next pick will skip this key via `WHERE enabled = true`.
				// We also set a long cooldown as a belt-and-suspenders measure
				// so even a stale in-memory read won't route to it.
				dbMu.Lock()
				disableKey(key.ID)
				dbMu.Unlock()
				log.Printf("quota check: key %d (%s) credential expired — disabled", key.ID, group)
			}
		}(i, k)
	}

	wg.Wait()
	return results
}

// disableKey marks a key as disabled and sets a far-future cooldown so the
// Picker definitively skips it even if another goroutine is mid-pick.
func disableKey(keyID uint) {
	farFuture := time.Now().Add(365 * 24 * time.Hour)
	store.DB().Model(&store.Key{}).Where("id = ?", keyID).Updates(map[string]any{
		"enabled":        false,
		"cooldown_until": farFuture,
	})
}

// SummaryQuotaResults tallies a batch of QuotaCheckResult for logging.
func SummaryQuotaResults(results []QuotaCheckResult) (total, expired, errors int) {
	for _, r := range results {
		total++
		if r.Expired {
			expired++
		}
		if r.Error != nil {
			errors++
		}
	}
	return
}
