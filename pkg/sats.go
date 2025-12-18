package pkg

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
	p.mu.RLock()
	defer p.mu.RUnlock()

	used := p.usedSize()

	stats := PoolStats{
		Capacity:   p.capacity,
		Reserved:   p.reserved,
		Used:       used,
		Free:       p.capacity - used,
		CacheCount: len(p.caches),
		Caches:     make(map[string]CacheStats, len(p.caches)),
	}

	for name, info := range p.caches {
		total := info.hits + info.misses
		var hitRate float64
		if total > 0 {
			hitRate = float64(info.hits) / float64(total)
		}

		stats.Caches[name] = CacheStats{
			Name:      name,
			Size:      info.size,
			Min:       info.min,
			Max:       info.max,
			Priority:  info.priority,
			Hits:      info.hits,
			Misses:    info.misses,
			Evictions: info.evictions,
			HitRate:   hitRate,
		}
	}

	return stats
}

// Stats returns statistics for this specific cache
func (c *Cache[K, V]) Stats() CacheStats {
	c.pool.mu.RLock()
	defer c.pool.mu.RUnlock()

	info := c.pool.caches[c.name]
	if info == nil {
		return CacheStats{Name: c.name}
	}

	total := info.hits + info.misses
	var hitRate float64
	if total > 0 {
		hitRate = float64(info.hits) / float64(total)
	}

	return CacheStats{
		Name:      info.name,
		Size:      info.size,
		Min:       info.min,
		Max:       info.max,
		Priority:  info.priority,
		Hits:      info.hits,
		Misses:    info.misses,
		Evictions: info.evictions,
		HitRate:   hitRate,
	}
}

// Size returns current number of entries in this cache
func (c *Cache[K, V]) Size() int {
	c.pool.mu.RLock()
	defer c.pool.mu.RUnlock()
	info := c.pool.caches[c.name]
	if info == nil {
		return 0
	}
	return info.size
}
