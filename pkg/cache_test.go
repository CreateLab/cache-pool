package pkg

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBasicSetGet(t *testing.T) {
	pool := NewPool(100)

	cache, err := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	cache.Set("foo", 42)

	val, ok := cache.Get("foo")
	if !ok {
		t.Fatal("expected to find key")
	}
	if val != 42 {
		t.Fatalf("expected 42, got %d", val)
	}

	// non-existent key
	_, ok = cache.Get("bar")
	if ok {
		t.Fatal("expected not to find key")
	}
}

func TestTTLExpiration(t *testing.T) {
	pool := NewPool(100)

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      50 * time.Millisecond,
		Priority: 1,
	})

	cache.Set("foo", 42)

	// should exist
	if _, ok := cache.Get("foo"); !ok {
		t.Fatal("expected key to exist")
	}

	// wait for expiration
	time.Sleep(60 * time.Millisecond)

	// should be expired
	if _, ok := cache.Get("foo"); ok {
		t.Fatal("expected key to be expired")
	}
}

func TestLRUEviction(t *testing.T) {
	pool := NewPool(5)

	cache, _ := Register[int, string](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      5,
		TTL:      time.Minute,
		Priority: 1,
	})

	// fill cache
	for i := 0; i < 5; i++ {
		cache.Set(i, "val")
	}

	if cache.Size() != 5 {
		t.Fatalf("expected size 5, got %d", cache.Size())
	}

	// add one more - should evict LRU (key 0)
	cache.Set(100, "new")

	if cache.Size() != 5 {
		t.Fatalf("expected size 5, got %d", cache.Size())
	}

	// key 0 should be evicted
	if _, ok := cache.Get(0); ok {
		t.Fatal("expected key 0 to be evicted")
	}

	// key 100 should exist
	if _, ok := cache.Get(100); !ok {
		t.Fatal("expected key 100 to exist")
	}
}

func TestPriorityEviction(t *testing.T) {
	pool := NewPool(10)

	// high priority cache
	highPrio, _ := Register[string, int](pool, Config{
		Name:     "high",
		Min:      2,
		Max:      8,
		TTL:      time.Minute,
		Priority: 10, // higher number = higher priority
	})

	// low priority cache
	lowPrio, _ := Register[string, int](pool, Config{
		Name:     "low",
		Min:      2,
		Max:      8,
		TTL:      time.Minute,
		Priority: 1,
	})

	// fill low priority cache (4 items)
	for i := 0; i < 4; i++ {
		lowPrio.Set(strconv.Itoa(i), i)
	}

	// fill high priority cache (6 items) - should evict from low priority
	for i := 0; i < 6; i++ {
		highPrio.Set(strconv.Itoa(i), i)
	}

	// pool is full (10 items)
	stats := pool.Stats()
	if stats.Used != 10 {
		t.Fatalf("expected 10 used, got %d", stats.Used)
	}

	// high priority should have all 6
	if highPrio.Size() != 6 {
		t.Fatalf("expected high prio size 6, got %d", highPrio.Size())
	}

	// low priority should have 4
	if lowPrio.Size() != 4 {
		t.Fatalf("expected low prio size 4, got %d", lowPrio.Size())
	}

	// now add more to high priority - should evict from low (until low hits min)
	for i := 6; i < 8; i++ {
		highPrio.Set(strconv.Itoa(i), i)
	}

	// low should be at min (2)
	if lowPrio.Size() != 2 {
		t.Fatalf("expected low prio at min (2), got %d", lowPrio.Size())
	}

	// high should be at 8
	if highPrio.Size() != 8 {
		t.Fatalf("expected high prio size 8, got %d", highPrio.Size())
	}
}

func TestMinProtection(t *testing.T) {
	pool := NewPool(10)

	cache1, _ := Register[string, int](pool, Config{
		Name:     "cache1",
		Min:      5,
		Max:      10,
		TTL:      time.Minute,
		Priority: 1,
	})

	cache2, _ := Register[string, int](pool, Config{
		Name:     "cache2",
		Min:      5,
		Max:      5,
		TTL:      time.Minute,
		Priority: 2,
	})

	// fill cache1 to min
	for i := 0; i < 5; i++ {
		cache1.Set(strconv.Itoa(i), i)
	}

	// fill cache2 to max
	for i := 0; i < 5; i++ {
		cache2.Set(strconv.Itoa(i), i)
	}

	// both at their limits
	if cache1.Size() != 5 {
		t.Fatalf("expected cache1 size 5, got %d", cache1.Size())
	}
	if cache2.Size() != 5 {
		t.Fatalf("expected cache2 size 5, got %d", cache2.Size())
	}

	// cache2 tries to add more - should evict from itself (at max and cache1 at min)
	cache2.Set("extra", 999)

	// cache1 should still have 5 (protected by min)
	if cache1.Size() != 5 {
		t.Fatalf("cache1 should be protected, got size %d", cache1.Size())
	}

	// cache2 should still have 5 (evicted one, added one)
	if cache2.Size() != 5 {
		t.Fatalf("expected cache2 size 5, got %d", cache2.Size())
	}
}

func TestGetOrLoad(t *testing.T) {
	pool := NewPool(100)

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	loadCount := 0
	loader := func() (int, error) {
		loadCount++
		return 42, nil
	}

	// first call - should load
	val, err := cache.GetOrLoad("foo", loader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 42 {
		t.Fatalf("expected 42, got %d", val)
	}
	if loadCount != 1 {
		t.Fatalf("expected 1 load, got %d", loadCount)
	}

	// second call - should use cache
	val, err = cache.GetOrLoad("foo", loader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 42 {
		t.Fatalf("expected 42, got %d", val)
	}
	if loadCount != 1 {
		t.Fatalf("expected still 1 load, got %d", loadCount)
	}
}

func TestGetOrLoadError(t *testing.T) {
	pool := NewPool(100)

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	expectedErr := errors.New("load failed")
	loader := func() (int, error) {
		return 0, expectedErr
	}

	_, err := cache.GetOrLoad("foo", loader)
	if err != expectedErr {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}

	// should not be cached
	if _, ok := cache.Get("foo"); ok {
		t.Fatal("error result should not be cached")
	}
}

func TestGetOrLoadSingleflight(t *testing.T) {
	pool := NewPool(100)

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	var loadCount int64
	loader := func() (int, error) {
		atomic.AddInt64(&loadCount, 1)
		time.Sleep(50 * time.Millisecond)
		return 42, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, _ := cache.GetOrLoad("foo", loader)
			if val != 42 {
				t.Errorf("expected 42, got %d", val)
			}
		}()
	}

	wg.Wait()

	// should only load once due to singleflight
	if loadCount != 1 {
		t.Fatalf("expected 1 load (singleflight), got %d", loadCount)
	}
}

func TestValidation(t *testing.T) {
	pool := NewPool(100)

	// sum(min) > capacity
	_, err := Register[string, int](pool, Config{
		Name:     "big",
		Min:      101,
		Max:      101,
		TTL:      time.Minute,
		Priority: 1,
	})
	if !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("expected ErrPoolCapacity, got %v", err)
	}

	// max > available
	Register[string, int](pool, Config{
		Name:     "first",
		Min:      50,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	_, err = Register[string, int](pool, Config{
		Name:     "second",
		Min:      10,
		Max:      60, // 60 > 100-50=50
		TTL:      time.Minute,
		Priority: 1,
	})
	if !errors.Is(err, ErrMaxExceedsAvail) {
		t.Fatalf("expected ErrMaxExceedsAvail, got %v", err)
	}
}

func TestStats(t *testing.T) {
	pool := NewPool(100)

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	cache.Set("foo", 1)
	cache.Set("bar", 2)

	cache.Get("foo") // hit
	cache.Get("baz") // miss

	stats := cache.Stats()
	if stats.Size != 2 {
		t.Fatalf("expected size 2, got %d", stats.Size)
	}
	if stats.Hits != 1 {
		t.Fatalf("expected 1 hit, got %d", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Fatalf("expected 1 miss, got %d", stats.Misses)
	}

	poolStats := pool.Stats()
	if poolStats.Used != 2 {
		t.Fatalf("expected pool used 2, got %d", poolStats.Used)
	}
}

func TestUpdateExisting(t *testing.T) {
	pool := NewPool(100)

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	cache.Set("foo", 1)
	cache.Set("foo", 2) // update

	val, ok := cache.Get("foo")
	if !ok {
		t.Fatal("expected key to exist")
	}
	if val != 2 {
		t.Fatalf("expected 2, got %d", val)
	}

	// size should still be 1
	if cache.Size() != 1 {
		t.Fatalf("expected size 1, got %d", cache.Size())
	}
}

func TestGetOrLoadPanic(t *testing.T) {
	pool := NewPool(100)

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	loader := func() (int, error) {
		panic("loader panic!")
	}

	// should not panic, should return error
	_, err := cache.GetOrLoad("foo", loader)
	if err == nil {
		t.Fatal("expected error from panicking loader")
	}
	if err.Error() != "loader panic: loader panic!" {
		t.Fatalf("unexpected error message: %v", err)
	}

	// should not be cached
	if _, ok := cache.Get("foo"); ok {
		t.Fatal("panic result should not be cached")
	}

	// subsequent calls should work
	val, err := cache.GetOrLoad("foo", func() (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 42 {
		t.Fatalf("expected 42, got %d", val)
	}
}

func TestConcurrentAccess(t *testing.T) {
	pool := NewPool(1000)

	cache, _ := Register[int, int](pool, Config{
		Name:     "test",
		Min:      100,
		Max:      500,
		TTL:      time.Minute,
		Priority: 1,
	})

	var wg sync.WaitGroup
	const goroutines = 100
	const opsPerGoroutine = 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := base*opsPerGoroutine + j
				cache.Set(key, key*2)
				cache.Get(key)
				cache.GetOrLoad(key+10000, func() (int, error) {
					return key * 3, nil
				})
			}
		}(i)
	}

	wg.Wait()

	// just verify no panics and stats are reasonable
	stats := cache.Stats()
	if stats.Hits+stats.Misses == 0 {
		t.Fatal("expected some operations")
	}
}

func TestBackgroundGC(t *testing.T) {
	pool := NewPoolWithGC(100, 30*time.Millisecond)
	defer pool.Close()

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      50 * time.Millisecond,
		Priority: 1,
	})

	// add some entries
	for i := 0; i < 10; i++ {
		cache.Set(strconv.Itoa(i), i)
	}

	if cache.Size() != 10 {
		t.Fatalf("expected size 10, got %d", cache.Size())
	}

	// wait for TTL + GC interval
	time.Sleep(100 * time.Millisecond)

	// GC should have cleaned up expired entries
	if cache.Size() != 0 {
		t.Fatalf("expected size 0 after GC, got %d", cache.Size())
	}
}

func TestMapShrinkAfterGC(t *testing.T) {
	pool := NewPoolWithGC(1000, 20*time.Millisecond)
	defer pool.Close()

	cache, _ := Register[int, [1024]byte](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      1000,
		TTL:      30 * time.Millisecond,
		Priority: 1,
	})

	// fill with data
	for i := 0; i < 100; i++ {
		cache.Set(i, [1024]byte{})
	}

	if cache.Size() != 100 {
		t.Fatalf("expected 100, got %d", cache.Size())
	}

	// wait for expiration + GC
	time.Sleep(80 * time.Millisecond)

	// should be empty and map recreated
	if cache.Size() != 0 {
		t.Fatalf("expected 0, got %d", cache.Size())
	}

	// verify map was recreated (internal check)
	pool.mu.RLock()
	mapLen := len(pool.storage)
	pool.mu.RUnlock()

	if mapLen != 0 {
		t.Fatalf("expected empty map, got %d", mapLen)
	}
}

func BenchmarkSet(b *testing.B) {
	pool := NewPool(100000)
	cache, _ := Register[int, int](pool, Config{
		Name:     "bench",
		Min:      1000,
		Max:      50000,
		TTL:      time.Minute,
		Priority: 1,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Set(i%10000, i)
	}
}

func BenchmarkGet(b *testing.B) {
	pool := NewPool(100000)
	cache, _ := Register[int, int](pool, Config{
		Name:     "bench",
		Min:      1000,
		Max:      50000,
		TTL:      time.Minute,
		Priority: 1,
	})

	for i := 0; i < 10000; i++ {
		cache.Set(i, i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Get(i % 10000)
	}
}

func BenchmarkGetOrLoad(b *testing.B) {
	pool := NewPool(100000)
	cache, _ := Register[int, int](pool, Config{
		Name:     "bench",
		Min:      1000,
		Max:      50000,
		TTL:      time.Minute,
		Priority: 1,
	})

	loader := func() (int, error) { return 42, nil }

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.GetOrLoad(i%10000, loader)
	}
}
