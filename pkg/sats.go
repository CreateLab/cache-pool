package pkg

import "sync/atomic"

// CacheStats holds metrics for a single cache
type CacheStats struct {
	Name      string
	Size      int
	Min       int
	Max       int
	Priority  int
	Hits      int64
	Misses    int64
	Evictions int64
	HitRate   float64 // hits / (hits + misses)
}

// PoolStats holds metrics for entire pool
type PoolStats struct {
	Capacity   int
	Reserved   int // sum of all min values
	Used       int // actual entries count
	Free       int // capacity - used
	CacheCount int
	Caches     map[string]CacheStats
}

// Stats returns current pool statistics
func (p *Pool) Stats() PoolStats {
	used := int(atomic.LoadInt32(&p.size))
	capacity := int(atomic.LoadInt32(&p.capacity))
	reserved := int(atomic.LoadInt32(&p.reserved))

	p.cachesMu.RLock()
	cacheCount := len(p.caches)
	caches := make(map[string]CacheStats, cacheCount)

	for name, info := range p.caches {
		hits := atomic.LoadInt64(&info.hits)
		misses := atomic.LoadInt64(&info.misses)
		total := hits + misses
		var hitRate float64
		if total > 0 {
			hitRate = float64(hits) / float64(total)
		}

		caches[name] = CacheStats{
			Name:      name,
			Size:      int(atomic.LoadInt32(&info.size)),
			Min:       info.min,
			Max:       info.max,
			Priority:  info.priority,
			Hits:      hits,
			Misses:    misses,
			Evictions: atomic.LoadInt64(&info.evictions),
			HitRate:   hitRate,
		}
	}
	p.cachesMu.RUnlock()

	return PoolStats{
		Capacity:   capacity,
		Reserved:   reserved,
		Used:       used,
		Free:       capacity - used,
		CacheCount: cacheCount,
		Caches:     caches,
	}
}

// Stats returns statistics for this specific cache
func (c *Cache[K, V]) Stats() CacheStats {
	c.pool.cachesMu.RLock()
	info := c.pool.caches[c.name]
	c.pool.cachesMu.RUnlock()

	if info == nil {
		return CacheStats{Name: c.name}
	}

	hits := atomic.LoadInt64(&info.hits)
	misses := atomic.LoadInt64(&info.misses)
	total := hits + misses
	var hitRate float64
	if total > 0 {
		hitRate = float64(hits) / float64(total)
	}

	return CacheStats{
		Name:      info.name,
		Size:      int(atomic.LoadInt32(&info.size)),
		Min:       info.min,
		Max:       info.max,
		Priority:  info.priority,
		Hits:      hits,
		Misses:    misses,
		Evictions: atomic.LoadInt64(&info.evictions),
		HitRate:   hitRate,
	}
}

// Size returns current number of entries in this cache
func (c *Cache[K, V]) Size() int {
	c.pool.cachesMu.RLock()
	info := c.pool.caches[c.name]
	c.pool.cachesMu.RUnlock()

	if info == nil {
		return 0
	}
	return int(atomic.LoadInt32(&info.size))
}
