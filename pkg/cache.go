package pkg

import (
	"fmt"
	"sync"
	"time"
)

// Cache is a typed wrapper around pool storage
type Cache[K comparable, V any] struct {
	pool *Pool
	name string
	ttl  time.Duration

	// singleflight for GetOrLoad
	sfMu    sync.Mutex
	sfCalls map[K]*sfCall[V]
}

type sfCall[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// Register creates a new typed cache in the pool
func Register[K comparable, V any](pool *Pool, cfg Config) (*Cache[K, V], error) {
	if err := pool.registerCache(cfg); err != nil {
		return nil, err
	}

	return &Cache[K, V]{
		pool:    pool,
		name:    cfg.Name,
		ttl:     cfg.TTL,
		sfCalls: make(map[K]*sfCall[V]),
	}, nil
}

// MustRegister is like Register but panics on error
func MustRegister[K comparable, V any](pool *Pool, cfg Config) *Cache[K, V] {
	c, err := Register[K, V](pool, cfg)
	if err != nil {
		panic(err)
	}
	return c
}

// Get retrieves a value from cache
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()

	info := c.pool.caches[c.name]
	if info == nil {
		var zero V
		return zero, false
	}

	fullKey := makeKey(c.name, key)

	ent, exists := c.pool.storage[fullKey]
	if !exists {
		info.misses++
		var zero V
		return zero, false
	}

	// check TTL
	if time.Now().After(ent.expiresAt) {
		// expired - remove lazily
		c.pool.removeEntry(ent)
		info.misses++
		var zero V
		return zero, false
	}

	// move to front (most recently used)
	c.pool.lruList.MoveToFront(ent.element)
	info.hits++

	val, ok := ent.value.(V)
	if !ok {
		info.misses++
		var zero V
		return zero, false
	}

	return val, true
}

// Set adds or updates a value in cache
func (c *Cache[K, V]) Set(key K, value V) {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()

	c.pool.setInternal(c.name, key, value, c.ttl)
}

// GetOrLoad gets from cache or loads using provided function (with singleflight)
func (c *Cache[K, V]) GetOrLoad(key K, loader func() (V, error)) (V, error) {
	// singleflight: ensure only one loader runs per key
	c.sfMu.Lock()

	// double-check: maybe value already in cache
	if val, ok := c.getInternal(key); ok {
		c.sfMu.Unlock()
		return val, nil
	}

	// check if another goroutine is already loading
	if call, ok := c.sfCalls[key]; ok {
		c.sfMu.Unlock()
		call.wg.Wait()
		return call.val, call.err
	}

	// we are the loader
	call := &sfCall[V]{}
	call.wg.Add(1)
	c.sfCalls[key] = call
	c.sfMu.Unlock()

	// run loader with panic recovery
	func() {
		defer func() {
			if r := recover(); r != nil {
				call.err = fmt.Errorf("loader panic: %v", r)
			}
		}()
		call.val, call.err = loader()
	}()

	if call.err == nil {
		c.Set(key, call.val)
	}

	// cleanup singleflight and signal waiters
	c.sfMu.Lock()
	delete(c.sfCalls, key)
	c.sfMu.Unlock()

	call.wg.Done()

	return call.val, call.err
}

// getInternal is like Get but without locking (caller must hold appropriate lock)
func (c *Cache[K, V]) getInternal(key K) (V, bool) {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()

	info := c.pool.caches[c.name]
	if info == nil {
		var zero V
		return zero, false
	}

	fullKey := makeKey(c.name, key)

	ent, exists := c.pool.storage[fullKey]
	if !exists {
		info.misses++
		var zero V
		return zero, false
	}

	// check TTL
	if time.Now().After(ent.expiresAt) {
		c.pool.removeEntry(ent)
		info.misses++
		var zero V
		return zero, false
	}

	c.pool.lruList.MoveToFront(ent.element)
	info.hits++

	val, ok := ent.value.(V)
	if !ok {
		info.misses++
		var zero V
		return zero, false
	}

	return val, true
}

// Name returns cache name
func (c *Cache[K, V]) Name() string {
	return c.name
}
