package pkg

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrInvalidConfig   = errors.New("invalid cache config")
	ErrPoolCapacity    = errors.New("pool capacity exceeded: sum(min) > capacity")
	ErrMaxExceedsAvail = errors.New("max exceeds available capacity: capacity - sum(other min) < max")
)

// Pool manages shared memory for multiple caches
type Pool struct {
	mu sync.RWMutex

	capacity int // total capacity limit
	reserved int // sum of all min values

	// unified storage
	storage map[string]*entry // key format: "cacheName:actualKey"
	lruList *list.List        // global LRU list

	// registered caches
	caches map[string]*cacheInfo

	// background GC
	gcStop chan struct{}
	gcDone chan struct{}
}

type entry struct {
	key       string // full key with prefix
	value     any
	cacheName string
	expiresAt time.Time
	element   *list.Element // pointer to LRU list element
}

type cacheInfo struct {
	name     string
	min      int
	max      int
	ttl      time.Duration
	priority int

	// current state
	size int

	// metrics
	hits      int64
	misses    int64
	evictions int64
}

// Config for registering a cache
type Config struct {
	Name     string
	Min      int
	Max      int
	TTL      time.Duration
	Priority int
}

func (c Config) validate() error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	if c.Min < 0 {
		return errors.New("min must be >= 0")
	}
	if c.Max < c.Min {
		return errors.New("max must be >= min")
	}
	if c.TTL <= 0 {
		return errors.New("ttl must be > 0")
	}
	return nil
}

// NewPool creates a new cache pool with given capacity
func NewPool(capacity int) *Pool {
	return &Pool{
		capacity: capacity,
		storage:  make(map[string]*entry),
		lruList:  list.New(),
		caches:   make(map[string]*cacheInfo),
	}
}

// NewPoolWithGC creates a new cache pool with background garbage collection
func NewPoolWithGC(capacity int, gcInterval time.Duration) *Pool {
	p := NewPool(capacity)
	p.gcStop = make(chan struct{})
	p.gcDone = make(chan struct{})

	go p.runGC(gcInterval)

	return p
}

// runGC periodically cleans up expired entries
func (p *Pool) runGC(interval time.Duration) {
	defer close(p.gcDone)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.gcStop:
			return
		case <-ticker.C:
			p.cleanupExpired()
		}
	}
}

// cleanupExpired removes all expired entries
func (p *Pool) cleanupExpired() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	count := 0
	now := time.Now()

	// collect expired entries first to avoid modifying list while iterating
	var toRemove []*entry
	for el := p.lruList.Back(); el != nil; el = el.Prev() {
		ent, ok := el.Value.(*entry)
		if !ok {
			continue
		}
		if now.After(ent.expiresAt) {
			toRemove = append(toRemove, ent)
		}
	}

	for _, ent := range toRemove {
		p.removeEntry(ent)
		if info, ok := p.caches[ent.cacheName]; ok {
			info.evictions++
		}
		count++
	}

	return count
}

// Close stops background GC and releases resources
func (p *Pool) Close() {
	if p.gcStop != nil {
		close(p.gcStop)
		<-p.gcDone
	}
}

// validateRegistration checks if new cache can be registered
func (p *Pool) validateRegistration(cfg Config) error {
	if err := cfg.validate(); err != nil {
		return errors.Join(ErrInvalidConfig, err)
	}

	if _, exists := p.caches[cfg.Name]; exists {
		return errors.Join(ErrInvalidConfig, errors.New("cache with this name already exists"))
	}

	// sum(min) <= capacity
	newReserved := p.reserved + cfg.Min
	if newReserved > p.capacity {
		return ErrPoolCapacity
	}

	// capacity - sum(other min) >= max
	// i.e., this cache's max shouldn't exceed what's available after reserving others' mins
	availableForThisCache := p.capacity - p.reserved // other caches' reserved
	if cfg.Max > availableForThisCache {
		return ErrMaxExceedsAvail
	}

	return nil
}

// registerCache adds cache metadata to pool (called by Register)
func (p *Pool) registerCache(cfg Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.validateRegistration(cfg); err != nil {
		return err
	}

	p.caches[cfg.Name] = &cacheInfo{
		name:     cfg.Name,
		min:      cfg.Min,
		max:      cfg.Max,
		ttl:      cfg.TTL,
		priority: cfg.Priority,
	}
	p.reserved += cfg.Min

	return nil
}

// makeKey creates unified storage key
func makeKey(cacheName string, key any) string {
	// Simple approach: use %v formatting
	// For production might want something more efficient
	return cacheName + ":" + toString(key)
}

func toString(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case int:
		return itoa(val)
	case int64:
		return itoa64(val)
	default:
		// fallback for other comparable types
		return fmt.Sprintf("%v", val)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// setInternal adds or updates entry (must be called with lock held)
func (p *Pool) setInternal(cacheName string, key any, value any, ttl time.Duration) {
	fullKey := makeKey(cacheName, key)
	info := p.caches[cacheName]

	// check if key already exists
	if ent, exists := p.storage[fullKey]; exists {
		// update existing
		ent.value = value
		ent.expiresAt = time.Now().Add(ttl)
		p.lruList.MoveToFront(ent.element)
		return
	}

	// need to add new entry
	// first check if this cache is at max
	if info.size >= info.max {
		// must evict from self
		p.evictFromCache(cacheName)
	} else if p.usedSize() >= p.capacity {
		// pool is full, need to evict
		p.evictOne(cacheName)
	}

	// create new entry
	ent := &entry{
		key:       fullKey,
		value:     value,
		cacheName: cacheName,
		expiresAt: time.Now().Add(ttl),
	}
	ent.element = p.lruList.PushFront(ent)
	p.storage[fullKey] = ent
	info.size++
}

// usedSize returns total number of entries across all caches
func (p *Pool) usedSize() int {
	total := 0
	for _, info := range p.caches {
		total += info.size
	}
	return total
}

// removeEntry removes an entry from storage (must be called with lock held)
func (p *Pool) removeEntry(ent *entry) {
	delete(p.storage, ent.key)
	p.lruList.Remove(ent.element)
	if info, ok := p.caches[ent.cacheName]; ok {
		info.size--
	}
}

// evictOne frees one slot using eviction policy
// priority: 1) expired anywhere 2) LRU from lowest priority cache above min 3) LRU from self
func (p *Pool) evictOne(requestingCache string) {
	// 1. try to find and remove any expired entry
	if p.evictExpired() {
		return
	}

	// 2. find cache with lowest priority that is above its min
	// (excluding requesting cache for now)
	var victim *cacheInfo
	for _, info := range p.caches {
		if info.name == requestingCache {
			continue
		}
		if info.size <= info.min {
			continue
		}
		if victim == nil || info.priority < victim.priority {
			victim = info
		}
	}

	if victim != nil {
		p.evictLRUFromCache(victim.name)
		return
	}

	// 3. all other caches at min, evict from self
	p.evictFromCache(requestingCache)
}

// evictExpired finds and removes one expired entry
func (p *Pool) evictExpired() bool {
	now := time.Now()
	// iterate from back (LRU end) to find expired
	for el := p.lruList.Back(); el != nil; el = el.Prev() {
		ent, ok := el.Value.(*entry)
		if !ok {
			continue
		}
		if now.After(ent.expiresAt) {
			p.removeEntry(ent)
			if info, ok := p.caches[ent.cacheName]; ok {
				info.evictions++
			}
			return true
		}
	}
	return false
}

// evictFromCache evicts LRU entry from specific cache
func (p *Pool) evictFromCache(cacheName string) {
	p.evictLRUFromCache(cacheName)
}

// evictLRUFromCache removes the least recently used entry from a specific cache
func (p *Pool) evictLRUFromCache(cacheName string) {
	// find LRU entry belonging to this cache
	for el := p.lruList.Back(); el != nil; el = el.Prev() {
		ent, ok := el.Value.(*entry)
		if !ok {
			continue
		}
		if ent.cacheName == cacheName {
			p.removeEntry(ent)
			if info, ok := p.caches[cacheName]; ok {
				info.evictions++
			}
			return
		}
	}
}
