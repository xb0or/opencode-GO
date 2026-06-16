package pool

import (
	"errors"
	"math"
	"sync"
	"time"

	"github.com/opencode-sw/gateway/store"
	"gorm.io/gorm"
)

// ErrNoAvailableKey means no enabled, non-cooled-down key exists for the group.
var ErrNoAvailableKey = errors.New("no available key for group")

// Picker is the concurrency-safe KEY pool with weighted round-robin scheduling,
// exponential-backoff failure cooldown, and usage bookkeeping.
type Picker struct {
	mu       sync.Mutex
	cursor   map[string]int64 // group -> virtual cursor (weighted round-robin)
	coolConf cooldownConfig
}

type cooldownConfig struct {
	threshold    int           // fail count to trigger first cooldown
	baseDuration time.Duration // initial cooldown length
	maxDuration  time.Duration // upper bound for exponential backoff
}

// NewPicker creates a Picker with exponential backoff cooldown:
//
//	fail >= threshold  →  baseDuration
//	each subsequent fail doubles the cooldown, capped at maxDuration.
func NewPicker() *Picker {
	return &Picker{
		cursor: map[string]int64{},
		coolConf: cooldownConfig{
			threshold:    3,
			baseDuration: 30 * time.Second,
			maxDuration:  10 * time.Minute,
		},
	}
}

// Pick selects the next available key for a group using weighted round-robin,
// skipping disabled keys and those in cooldown. Keys with higher Weight get
// proportionally more traffic. The returned key's LastUsed/UsageCount are
// updated. Returns ErrNoAvailableKey if none usable.
func (p *Picker) Pick(group string) (*store.Key, error) {
	keys, err := p.PickAttempts(group)
	if err != nil {
		return nil, err
	}
	chosen := &keys[0]
	p.MarkUsed(chosen.ID)
	chosen.UsageCount++
	now := time.Now()
	chosen.LastUsed = &now
	return chosen, nil
}

// PickAttempts returns all currently available keys for a group. The first key
// follows the weighted round-robin scheduler, and the remaining keys are
// returned once each as fallback candidates for the same request.
func (p *Picker) PickAttempts(group string) ([]store.Key, error) {
	if group == "" {
		group = "go"
	}

	var keys []store.Key
	if err := store.DB().Where("enabled = ? AND `group` = ?", true, group).Order("id asc").Find(&keys).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNoAvailableKey
		}
		return nil, err
	}
	if len(keys) == 0 {
		return nil, ErrNoAvailableKey
	}

	now := time.Now()

	// Calculate total effective weight (only keys not in cooldown).
	type candidate struct {
		index  int
		key    store.Key
		weight int
	}
	var candidates []candidate
	totalWeight := 0
	for i, k := range keys {
		if k.CooldownUntil != nil && k.CooldownUntil.After(now) {
			continue
		}
		w := k.Weight
		if w <= 0 {
			w = 1
		}
		candidates = append(candidates, candidate{index: i, key: k, weight: w})
		totalWeight += w
	}

	if len(candidates) == 0 {
		return nil, ErrNoAvailableKey
	}

	// Weighted round-robin: advance virtual cursor and map to candidate.
	p.mu.Lock()
	cursor := p.cursor[group]
	p.cursor[group] = cursor + 1
	p.mu.Unlock()

	// Map cursor to a candidate using weighted distribution.
	// We cycle through totalWeight positions.
	pos := cursor % int64(totalWeight)
	if pos < 0 {
		pos += int64(totalWeight)
	}

	chosenIndex := -1
	accumulated := int64(0)
	for i, c := range candidates {
		accumulated += int64(c.weight)
		if pos < accumulated {
			chosenIndex = i
			break
		}
	}
	if chosenIndex < 0 {
		// Fallback: pick first candidate.
		chosenIndex = 0
	}

	attempts := make([]store.Key, 0, len(candidates))
	for offset := 0; offset < len(candidates); offset++ {
		c := candidates[(chosenIndex+offset)%len(candidates)]
		attempts = append(attempts, c.key)
	}
	return attempts, nil
}

// MarkUsed updates usage bookkeeping for a key that is about to be attempted.
func (p *Picker) MarkUsed(keyID uint) {
	now := time.Now()
	store.DB().Model(&store.Key{}).Where("id = ?", keyID).Updates(map[string]any{
		"last_used":   now,
		"usage_count": gorm.Expr("usage_count + ?", 1),
	})
}

// PickAll returns every available (enabled + not cooled-down) key for a group.
// Useful when you want to try multiple keys on failure.
func (p *Picker) PickAll(group string) ([]store.Key, error) {
	if group == "" {
		group = "go"
	}
	var keys []store.Key
	now := time.Now()
	err := store.DB().
		Where("enabled = ? AND `group` = ? AND (cooldown_until IS NULL OR cooldown_until <= ?)", true, group, now).
		Find(&keys).Error
	return keys, err
}

// MarkSuccess resets a key's fail counter and clears cooldown.
func (p *Picker) MarkSuccess(keyID uint) {
	store.DB().Model(&store.Key{}).Where("id = ?", keyID).Updates(map[string]any{
		"fail_count":     0,
		"cooldown_until": nil,
	})
}

// MarkFailure increments the fail counter with exponential backoff:
//
//	cooldown = min(baseDuration * 2^(fails-threshold), maxDuration)
func (p *Picker) MarkFailure(keyID uint) {
	var k store.Key
	if err := store.DB().First(&k, keyID).Error; err != nil {
		return
	}
	newCount := k.FailCount + 1
	updates := map[string]any{"fail_count": newCount}
	if newCount >= p.coolConf.threshold {
		exponent := float64(newCount - p.coolConf.threshold)
		dur := time.Duration(float64(p.coolConf.baseDuration) * math.Pow(2, exponent))
		if dur > p.coolConf.maxDuration {
			dur = p.coolConf.maxDuration
		}
		until := time.Now().Add(dur)
		updates["cooldown_until"] = until
	}
	store.DB().Model(&store.Key{}).Where("id = ?", k.ID).Updates(updates)
}

// ResetCooldown manually clears a key's fail count and cooldown.
func (p *Picker) ResetCooldown(keyID uint) {
	store.DB().Model(&store.Key{}).Where("id = ?", keyID).Updates(map[string]any{
		"fail_count":     0,
		"cooldown_until": nil,
	})
}

// AllByGroup returns all keys (including disabled) for inspection / admin.
func AllByGroup(group string) ([]store.Key, error) {
	var keys []store.Key
	q := store.DB().Order("id asc")
	if group != "" {
		q = q.Where("`group` = ?", group)
	}
	return keys, q.Find(&keys).Error
}

// PoolStats returns summary stats for a group.
type PoolStats struct {
	Total     int `json:"total"`
	Enabled   int `json:"enabled"`
	Available int `json:"available"`
	Cooldown  int `json:"cooldown"`
}

// Stats returns pool health stats for a group.
func (p *Picker) Stats(group string) PoolStats {
	var all []store.Key
	store.DB().Where("`group` = ?", group).Find(&all)

	now := time.Now()
	var s PoolStats
	s.Total = len(all)
	for _, k := range all {
		if !k.Enabled {
			continue
		}
		s.Enabled++
		if k.CooldownUntil != nil && k.CooldownUntil.After(now) {
			s.Cooldown++
			continue
		}
		s.Available++
	}
	return s
}
