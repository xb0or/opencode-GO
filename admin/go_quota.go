package admin

import (
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/store"
)

// CheckGoGroupQuotas runs concurrent quota checks for every enabled Go key
// in the pool, using the existing fetchGoQuota logic. Designed for background
// ticker use.
//
// maxConcurrency caps parallel HTTP checks. Go quota checks hit the opencode.ai
// API and are relatively fast, but unbounded parallelism with many keys could
// still trip rate limits.
func CheckGoGroupQuotas(p *pool.Picker, maxConcurrency int) []pool.QuotaCheckResult {
	return pool.CheckGroupQuotas(p, "go", checkGoKeyQuota, maxConcurrency)
}

// checkGoKeyQuota is the QuotaChecker callback for Go (opencode.ai) keys. It
// reuses fetchGoQuota and detects cookie expiry via isCookieExpiredError.
func checkGoKeyQuota(k store.Key) (expired bool, err error) {
	cookie := normalizeAuthCookie(k.Cookie)
	if cookie == "" {
		return true, nil // no cookie configured
	}
	workspaceID := normalizeWorkspaceID(k.WorkspaceID)
	if workspaceID == "" {
		// Auto-detect workspace (no quota fetch yet, just resolve).
		wid, _, err := resolveWorkspaceForQuota(cookie)
		if err != nil {
			if isCookieExpiredError(err) {
				return true, nil
			}
			return false, err
		}
		workspaceID = wid
		// Persist resolved workspace so next check is faster.
		store.DB().Model(&store.Key{}).Where("id = ?", k.ID).Update("workspace_id", workspaceID)
	}
	_, err = fetchGoQuota(cookie, workspaceID)
	if err != nil && isCookieExpiredError(err) {
		return true, nil
	}
	return false, err
}
