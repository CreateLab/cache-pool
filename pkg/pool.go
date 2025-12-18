package pkg

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrInvalidConfig   = errors.New("invalid cache config")
	ErrPoolCapacity    = errors.New("pool capacity exceeded: sum(min) > capacity")
	ErrMaxExceedsAvail = errors.New("max exceeds available capacity: capacity - sum(other min) < max")
)

const (
	defaultGCInterval     = time.Minute
	defaultPromoteBuffer  = 1024
	defaultPromoteWorkers = 1
)

// PoolOption configures Pool
type PoolOption func(*poolConfig)

type poolConfig struct {
	gcInterval     time.Duration
	promoteBuffer  int
	promoteWorkers int
}

// WithGCInterval sets garbage collection interval
func WithGCInterval(d time.Duration) PoolOption {
	return func(c *poolConfig) {
		c.gcInterval = d
	}
}

// WithPromoteBuffer sets promote channel buffer size
func WithPromoteBuffer(size int) PoolOption {
	return func(c *poolConfig) {
		c.promoteBuffer = size
	}
}

// Pool manages shared memory for multiple caches
type Pool struct {
	capacity int32 // total capacity limit
	reserved int32 // sum of all min values
	size     int32 // current total entries (atomic)

	// unified storage - lock-free reads
	storage sync.Map // map[string]*entry

	// LRU tracking (protected by lruMu)
	lruMu   sync.Mutex
	lruList *list.List

	// registered caches (protected by cachesMu)
	cachesMu sync.RWMutex
	caches   map[string]*cacheInfo

	// async promote
	promoteCh chan *entry

	// background workers
	stop chan struct{}
	done sync.WaitGroup
}

type entry struct {
	key       string
	value     any
	cacheName string
	expiresAt int64         // unix nano for atomic access
	element   *list.Element // protected by lruMu

	// для отслеживания удаления
	deleted int32 // atomic: 1 = deleted
}

func (e *entry) isExpired() bool {
	return time.Now().UnixNano() > atomic.LoadInt64(&e.expiresAt)
}

func (e *entry) isDeleted() bool {
	return atomic.LoadInt32(&e.deleted) == 1
}

func (e *entry) markDeleted() bool {
	return atomic.CompareAndSwapInt32(&e.deleted, 0, 1)
}

type cacheInfo struct {
	name     string
	min      int
	max      int
	ttl      time.Duration
	priority int

	// current state (atomic)
	size int32

	// metrics (atomic)
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
func NewPool(capacity int, opts ...PoolOption) *Pool {
	cfg := &poolConfig{
		gcInterval:     defaultGCInterval,
		promoteBuffer:  defaultPromoteBuffer,
		promoteWorkers: defaultPromoteWorkers,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	p := &Pool{
		capacity:  int32(capacity),
		lruList:   list.New(),
		caches:    make(map[string]*cacheInfo),
		promoteCh: make(chan *entry, cfg.promoteBuffer),
		stop:      make(chan struct{}),
	}

	// start GC worker
	p.done.Add(1)
	go p.gcWorker(cfg.gcInterval)

	// start promote workers
	for i := 0; i < cfg.promoteWorkers; i++ {
		p.done.Add(1)
		go p.promoteWorker()
	}

	return p
}

// gcWorker periodically cleans up expired entries
func (p *Pool) gcWorker(interval time.Duration) {
	defer p.done.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.cleanupExpired()
		}
	}
}

// promoteWorker processes LRU promotions
func (p *Pool) promoteWorker() {
	defer p.done.Done()
	for {
		select {
		case <-p.stop:
			return
		case ent := <-p.promoteCh:
			if ent.isDeleted() || ent.isExpired() {
				continue
			}
			p.lruMu.Lock()
			if ent.element != nil && !ent.isDeleted() {
				p.lruList.MoveToFront(ent.element)
			}
			p.lruMu.Unlock()
		}
	}
}

// cleanupExpired removes all expired entries
func (p *Pool) cleanupExpired() int {
	count := 0

	p.lruMu.Lock()
	var toRemove []*entry
	for el := p.lruList.Back(); el != nil; el = el.Prev() {
		ent, ok := el.Value.(*entry)
		if !ok {
			continue
		}
		if ent.isExpired() {
			toRemove = append(toRemove, ent)
		}
	}
	p.lruMu.Unlock()

	for _, ent := range toRemove {
		if ent.markDeleted() {
			p.removeEntry(ent)
			count++
		}
	}

	// shrink if empty
	if atomic.LoadInt32(&p.size) == 0 {
		p.lruMu.Lock()
		p.lruList = list.New()
		p.lruMu.Unlock()
	}

	return count
}

// Close stops background workers and releases resources
func (p *Pool) Close() {
	close(p.stop)
	p.done.Wait()
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
	newReserved := int(p.reserved) + cfg.Min
	if newReserved > int(p.capacity) {
		return ErrPoolCapacity
	}

	// capacity - sum(other min) >= max
	availableForThisCache := int(p.capacity) - int(p.reserved)
	if cfg.Max > availableForThisCache {
		return ErrMaxExceedsAvail
	}

	return nil
}

// registerCache adds cache metadata to pool
func (p *Pool) registerCache(cfg Config) error {
	p.cachesMu.Lock()
	defer p.cachesMu.Unlock()

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
	atomic.AddInt32(&p.reserved, int32(cfg.Min))

	return nil
}

// get retrieves entry without locking (lock-free)
func (p *Pool) get(fullKey string) (*entry, bool) {
	raw, exists := p.storage.Load(fullKey)
	if !exists {
		return nil, false
	}

	ent := raw.(*entry)
	if ent.isDeleted() || ent.isExpired() {
		return nil, false
	}

	return ent, true
}

// promote schedules LRU promotion (non-blocking)
func (p *Pool) promote(ent *entry) {
	select {
	case p.promoteCh <- ent:
	default:
		// channel full, skip promotion
	}
}

// setInternal adds or updates entry
func (p *Pool) setInternal(cacheName string, key any, value any, ttl time.Duration) {
	fullKey := makeKey(cacheName, key)

	p.cachesMu.RLock()
	info := p.caches[cacheName]
	p.cachesMu.RUnlock()

	if info == nil {
		return
	}

	expiresAt := time.Now().Add(ttl).UnixNano()

	// check if key already exists
	if raw, exists := p.storage.Load(fullKey); exists {
		ent := raw.(*entry)
		if !ent.isDeleted() {
			// update existing
			ent.value = value
			atomic.StoreInt64(&ent.expiresAt, expiresAt)
			p.promote(ent)
			return
		}
	}

	// need to add new entry
	cacheSize := atomic.LoadInt32(&info.size)

	// check if this cache is at max - evict until below max
	if int(cacheSize) >= info.max {
		for int(atomic.LoadInt32(&info.size)) >= info.max {
			if !p.evictFromCache(cacheName) {
				break // no more entries to evict from this cache
			}
		}
	}

	// check if pool is full - evict until below capacity
	// Используем цикл для гарантии освобождения места
	for atomic.LoadInt32(&p.size) >= p.capacity {
		if !p.evictOne(cacheName) {
			break // no more entries to evict
		}
	}

	// create new entry
	ent := &entry{
		key:       fullKey,
		value:     value,
		cacheName: cacheName,
		expiresAt: expiresAt,
	}

	// add to LRU list
	p.lruMu.Lock()
	ent.element = p.lruList.PushFront(ent)
	p.lruMu.Unlock()

	// add to storage
	p.storage.Store(fullKey, ent)
	atomic.AddInt32(&info.size, 1)
	atomic.AddInt32(&p.size, 1)
}

// removeEntry removes an entry from storage
func (p *Pool) removeEntry(ent *entry) {
	p.storage.Delete(ent.key)

	p.lruMu.Lock()
	if ent.element != nil {
		p.lruList.Remove(ent.element)
		ent.element = nil
	}
	p.lruMu.Unlock()

	p.cachesMu.RLock()
	info := p.caches[ent.cacheName]
	p.cachesMu.RUnlock()

	if info != nil {
		atomic.AddInt32(&info.size, -1)
		atomic.AddInt64(&info.evictions, 1)
	}
	atomic.AddInt32(&p.size, -1)
}

// evictOne frees one slot using eviction policy
// Returns true if an entry was evicted, false otherwise
func (p *Pool) evictOne(requestingCache string) bool {
	// 1. try to find and remove any expired entry
	if p.evictExpired() {
		return true
	}

	// 2. find cache with lowest priority above its min
	p.cachesMu.RLock()
	var victim *cacheInfo
	for _, info := range p.caches {
		if info.name == requestingCache {
			continue
		}
		if int(atomic.LoadInt32(&info.size)) <= info.min {
			continue
		}
		if victim == nil || info.priority < victim.priority {
			victim = info
		}
	}
	p.cachesMu.RUnlock()

	if victim != nil {
		return p.evictLRUFromCache(victim.name)
	}

	// 3. all other caches at min, evict from self
	return p.evictFromCache(requestingCache)
}

// evictExpired finds and removes one expired entry
func (p *Pool) evictExpired() bool {
	p.lruMu.Lock()
	var found *entry
	for el := p.lruList.Back(); el != nil; el = el.Prev() {
		ent, ok := el.Value.(*entry)
		if !ok {
			continue
		}
		if ent.isExpired() && !ent.isDeleted() {
			found = ent
			break
		}
	}
	p.lruMu.Unlock()

	if found != nil && found.markDeleted() {
		p.removeEntry(found)
		return true
	}
	return false
}

// evictFromCache evicts LRU entry from specific cache
// Returns true if an entry was evicted, false otherwise
func (p *Pool) evictFromCache(cacheName string) bool {
	return p.evictLRUFromCache(cacheName)
}

// evictLRUFromCache removes the least recently used entry from a specific cache
// Returns true if an entry was evicted, false otherwise
func (p *Pool) evictLRUFromCache(cacheName string) bool {
	p.lruMu.Lock()
	var found *entry
	for el := p.lruList.Back(); el != nil; el = el.Prev() {
		ent, ok := el.Value.(*entry)
		if !ok {
			continue
		}
		if ent.cacheName == cacheName && !ent.isDeleted() {
			found = ent
			break
		}
	}
	p.lruMu.Unlock()

	if found != nil && found.markDeleted() {
		p.removeEntry(found)
		return true
	}
	return false
}

// makeKey creates unified storage key
func makeKey(cacheName string, key any) string {
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
