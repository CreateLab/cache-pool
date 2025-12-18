package pkg

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Cache is a typed wrapper around pool storage
type Cache[K comparable, V any] struct {
	pool *Pool
	name string
	ttl  time.Duration
	info *cacheInfo // cached pointer, avoid map lookup on every Get

	// singleflight for GetOrLoad (per-key)
	sfCalls sync.Map // map[K]*sfCall[V]
}

type sfCall[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// BatchLoader configuration
type BatchConfig struct {
	MaxWait  time.Duration // max time to wait for batch (default 5ms)
	MaxBatch int           // max keys per batch (default 100)
}

type batcher[K comparable, V any] struct {
	cache  *Cache[K, V]
	loader func([]K) (map[K]V, error)
	config BatchConfig

	mu      sync.Mutex
	keys    []K
	callers []chan batchResult[V]
	timer   *time.Timer
}

type batchResult[V any] struct {
	val V
	ok  bool
	err error
}

// Register creates a new typed cache in the pool
func Register[K comparable, V any](pool *Pool, cfg Config) (*Cache[K, V], error) {
	if err := pool.registerCache(cfg); err != nil {
		return nil, err
	}

	pool.cachesMu.RLock()
	info := pool.caches[cfg.Name]
	pool.cachesMu.RUnlock()

	return &Cache[K, V]{
		pool: pool,
		name: cfg.Name,
		ttl:  cfg.TTL,
		info: info,
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

// Get retrieves a value from cache (lock-free)
func (c *Cache[K, V]) Get(key K) (V, bool) {
	fullKey := makeKey(c.name, key)

	ent, ok := c.pool.get(fullKey)
	if !ok {
		c.incMisses()
		var zero V
		return zero, false
	}

	// async promote (non-blocking)
	c.pool.promote(ent)

	c.incHits()

	val, ok := ent.value.(V)
	if !ok {
		var zero V
		return zero, false
	}

	return val, true
}

// Set adds or updates a value in cache
func (c *Cache[K, V]) Set(key K, value V) {
	c.pool.setInternal(c.name, key, value, c.ttl)
}

// GetOrLoad gets from cache or loads using provided function (with per-key singleflight)
func (c *Cache[K, V]) GetOrLoad(key K, loader func() (V, error)) (V, error) {
	// fast path: check cache first
	if val, ok := c.Get(key); ok {
		return val, nil
	}

	// singleflight per-key
	newCall := &sfCall[V]{}
	newCall.wg.Add(1)

	if existing, loaded := c.sfCalls.LoadOrStore(key, newCall); loaded {
		// another goroutine is loading this key
		call := existing.(*sfCall[V])
		call.wg.Wait()
		return call.val, call.err
	}

	// we are the loader
	call := newCall

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

	// cleanup and signal waiters
	c.sfCalls.Delete(key)
	call.wg.Done()

	return call.val, call.err
}

// GetOrLoadBatch creates a batching loader (DataLoader pattern)
func (c *Cache[K, V]) GetOrLoadBatch(loader func([]K) (map[K]V, error), cfg BatchConfig) func(K) (V, error) {
	if cfg.MaxWait == 0 {
		cfg.MaxWait = 5 * time.Millisecond
	}
	if cfg.MaxBatch == 0 {
		cfg.MaxBatch = 100
	}

	b := &batcher[K, V]{
		cache:  c,
		loader: loader,
		config: cfg,
	}

	return b.load
}

func (b *batcher[K, V]) load(key K) (V, error) {
	// fast path: check cache first
	if val, ok := b.cache.Get(key); ok {
		return val, nil
	}

	result := make(chan batchResult[V], 1)

	b.mu.Lock()

	// add to batch
	b.keys = append(b.keys, key)
	b.callers = append(b.callers, result)

	// start timer on first key
	if len(b.keys) == 1 {
		b.timer = time.AfterFunc(b.config.MaxWait, b.dispatch)
	}

	// dispatch immediately if batch is full
	if len(b.keys) >= b.config.MaxBatch {
		b.timer.Stop()
		go b.dispatch()
	}

	b.mu.Unlock()

	// wait for result
	res := <-result
	return res.val, res.err
}

func (b *batcher[K, V]) dispatch() {
	b.mu.Lock()
	keys := b.keys
	callers := b.callers
	b.keys = nil
	b.callers = nil
	b.timer = nil
	b.mu.Unlock()

	if len(keys) == 0 {
		return
	}

	// filter out keys already in cache (might have been added while waiting)
	var keysToLoad []K
	keyIndex := make(map[int]int) // original index -> keysToLoad index

	for i, key := range keys {
		if _, ok := b.cache.Get(key); !ok {
			keyIndex[i] = len(keysToLoad)
			keysToLoad = append(keysToLoad, key)
		}
	}

	// load missing keys
	var results map[K]V
	var loadErr error

	if len(keysToLoad) > 0 {
		results, loadErr = b.loader(keysToLoad)

		// cache results
		if loadErr == nil {
			for k, v := range results {
				b.cache.Set(k, v)
			}
		}
	}

	// send results to callers
	for i, key := range keys {
		var res batchResult[V]

		if loadErr != nil {
			res.err = loadErr
		} else if val, ok := b.cache.Get(key); ok {
			// might have been in cache or just loaded
			res.val = val
			res.ok = true
		} else if results != nil {
			if val, ok := results[key]; ok {
				res.val = val
				res.ok = true
			}
		}

		callers[i] <- res
	}
}

// Name returns cache name
func (c *Cache[K, V]) Name() string {
	return c.name
}

// incHits increments hit counter (atomic)
func (c *Cache[K, V]) incHits() {
	if c.info != nil {
		atomic.AddInt64(&c.info.hits, 1)
	}
}

// incMisses increments miss counter (atomic)
func (c *Cache[K, V]) incMisses() {
	if c.info != nil {
		atomic.AddInt64(&c.info.misses, 1)
	}
}
