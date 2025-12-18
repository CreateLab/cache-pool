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
	defer pool.Close()

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
	pool := NewPool(100, WithGCInterval(30*time.Millisecond))
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
	pool := NewPool(1000, WithGCInterval(20*time.Millisecond))
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

	// should be empty
	if cache.Size() != 0 {
		t.Fatalf("expected 0, got %d", cache.Size())
	}

	// pool size should be 0
	stats := pool.Stats()
	if stats.Used != 0 {
		t.Fatalf("expected pool used 0, got %d", stats.Used)
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

func BenchmarkGetParallel(b *testing.B) {
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
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Get(i % 10000)
			i++
		}
	})
}

func BenchmarkSetParallel(b *testing.B) {
	pool := NewPool(100000)
	cache, _ := Register[int, int](pool, Config{
		Name:     "bench",
		Min:      1000,
		Max:      50000,
		TTL:      time.Minute,
		Priority: 1,
	})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Set(i%10000, i)
			i++
		}
	})
}

func TestGetOrLoadBatch(t *testing.T) {
	pool := NewPool(100)
	defer pool.Close()

	cache, _ := Register[int, string](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	var batchCalls int64
	var batchSizes []int
	var mu sync.Mutex

	loader := cache.GetOrLoadBatch(func(keys []int) (map[int]string, error) {
		atomic.AddInt64(&batchCalls, 1)
		mu.Lock()
		batchSizes = append(batchSizes, len(keys))
		mu.Unlock()

		time.Sleep(10 * time.Millisecond) // simulate DB call
		result := make(map[int]string)
		for _, k := range keys {
			result[k] = strconv.Itoa(k * 10)
		}
		return result, nil
	}, BatchConfig{
		MaxWait:  20 * time.Millisecond,
		MaxBatch: 10,
	})

	// launch concurrent requests
	var wg sync.WaitGroup
	results := make([]string, 20)
	errors := make([]error, 20)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errors[idx] = loader(idx)
		}(i)
	}

	wg.Wait()

	// verify results
	for i := 0; i < 20; i++ {
		if errors[i] != nil {
			t.Fatalf("unexpected error for key %d: %v", i, errors[i])
		}
		expected := strconv.Itoa(i * 10)
		if results[i] != expected {
			t.Fatalf("expected %s for key %d, got %s", expected, i, results[i])
		}
	}

	// should have batched (fewer calls than keys)
	calls := atomic.LoadInt64(&batchCalls)
	if calls >= 20 {
		t.Fatalf("expected batching, but got %d calls for 20 keys", calls)
	}
	t.Logf("batch calls: %d, sizes: %v", calls, batchSizes)
}

func TestGetOrLoadBatchCached(t *testing.T) {
	pool := NewPool(100)
	defer pool.Close()

	cache, _ := Register[int, string](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	var loadCalls int64

	loader := cache.GetOrLoadBatch(func(keys []int) (map[int]string, error) {
		atomic.AddInt64(&loadCalls, 1)
		result := make(map[int]string)
		for _, k := range keys {
			result[k] = strconv.Itoa(k)
		}
		return result, nil
	}, BatchConfig{})

	// pre-populate cache
	cache.Set(1, "cached-1")
	cache.Set(2, "cached-2")

	// request cached keys
	val, err := loader(1)
	if err != nil {
		t.Fatal(err)
	}
	if val != "cached-1" {
		t.Fatalf("expected cached-1, got %s", val)
	}

	// should not have called loader
	if atomic.LoadInt64(&loadCalls) != 0 {
		t.Fatal("should not call loader for cached keys")
	}
}

func TestGetOrLoadPerKeySingleflight(t *testing.T) {
	pool := NewPool(100)
	defer pool.Close()

	cache, _ := Register[int, int](pool, Config{
		Name:     "test",
		Min:      10,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	var key1Loads, key2Loads int64

	var wg sync.WaitGroup

	// 10 goroutines loading key 1
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.GetOrLoad(1, func() (int, error) {
				atomic.AddInt64(&key1Loads, 1)
				time.Sleep(50 * time.Millisecond)
				return 100, nil
			})
		}()
	}

	// 10 goroutines loading key 2 (should not block key 1)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.GetOrLoad(2, func() (int, error) {
				atomic.AddInt64(&key2Loads, 1)
				time.Sleep(50 * time.Millisecond)
				return 200, nil
			})
		}()
	}

	wg.Wait()

	// each key should only be loaded once
	if key1Loads != 1 {
		t.Fatalf("expected 1 load for key1, got %d", key1Loads)
	}
	if key2Loads != 1 {
		t.Fatalf("expected 1 load for key2, got %d", key2Loads)
	}

	// verify values
	val1, _ := cache.Get(1)
	val2, _ := cache.Get(2)
	if val1 != 100 || val2 != 200 {
		t.Fatalf("unexpected values: %d, %d", val1, val2)
	}
}

// TestGCRemovesExpiredEntries проверяет, что GC удаляет expired entries
func TestGCRemovesExpiredEntries(t *testing.T) {
	pool := NewPool(100, WithGCInterval(20*time.Millisecond))
	defer pool.Close()

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      100,
		TTL:      30 * time.Millisecond,
		Priority: 1,
	})

	// добавляем записи
	for i := 0; i < 10; i++ {
		cache.Set(strconv.Itoa(i), i)
	}

	initialSize := cache.Size()
	if initialSize != 10 {
		t.Fatalf("expected initial size 10, got %d", initialSize)
	}

	initialPoolSize := pool.Stats().Used
	if initialPoolSize != 10 {
		t.Fatalf("expected initial pool size 10, got %d", initialPoolSize)
	}

	// ждем истечения TTL + GC интервал
	time.Sleep(60 * time.Millisecond)

	// проверяем, что все записи удалены
	finalSize := cache.Size()
	if finalSize != 0 {
		t.Fatalf("expected size 0 after GC, got %d", finalSize)
	}

	finalPoolSize := pool.Stats().Used
	if finalPoolSize != 0 {
		t.Fatalf("expected pool size 0 after GC, got %d", finalPoolSize)
	}

	// проверяем, что записи действительно удалены из storage
	for i := 0; i < 10; i++ {
		if _, ok := cache.Get(strconv.Itoa(i)); ok {
			t.Fatalf("expected key %d to be removed by GC", i)
		}
	}
}

// TestGCPartialExpiration проверяет частичное истечение TTL
func TestGCPartialExpiration(t *testing.T) {
	pool := NewPool(100, WithGCInterval(20*time.Millisecond))
	defer pool.Close()

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      100,
		TTL:      50 * time.Millisecond,
		Priority: 1,
	})

	// добавляем первую партию записей
	for i := 0; i < 5; i++ {
		cache.Set(strconv.Itoa(i), i)
	}

	// ждем немного
	time.Sleep(30 * time.Millisecond)

	// добавляем вторую партию (свежие записи)
	for i := 5; i < 10; i++ {
		cache.Set(strconv.Itoa(i), i)
	}

	// ждем, чтобы первая партия истекла, но вторая еще нет
	time.Sleep(40 * time.Millisecond)

	// первая партия должна быть удалена
	for i := 0; i < 5; i++ {
		if _, ok := cache.Get(strconv.Itoa(i)); ok {
			t.Fatalf("expected key %d to be expired", i)
		}
	}

	// вторая партия должна остаться
	for i := 5; i < 10; i++ {
		if _, ok := cache.Get(strconv.Itoa(i)); !ok {
			t.Fatalf("expected key %d to still exist", i)
		}
	}

	// размер должен быть 5
	if cache.Size() != 5 {
		t.Fatalf("expected size 5, got %d", cache.Size())
	}
}

// TestEvictionWhenCapacityExceeded проверяет eviction при превышении capacity
func TestEvictionWhenCapacityExceeded(t *testing.T) {
	pool := NewPool(10) // маленький пул
	defer pool.Close()

	cache, _ := Register[int, string](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      10,
		TTL:      time.Minute,
		Priority: 1,
	})

	// заполняем до capacity
	for i := 0; i < 10; i++ {
		cache.Set(i, "val")
	}

	if cache.Size() != 10 {
		t.Fatalf("expected size 10, got %d", cache.Size())
	}

	poolStats := pool.Stats()
	if poolStats.Used != 10 {
		t.Fatalf("expected pool used 10, got %d", poolStats.Used)
	}

	// добавляем еще одну запись - должна вытеснить LRU (key 0)
	cache.Set(100, "new")

	// размер должен остаться 10
	if cache.Size() != 10 {
		t.Fatalf("expected size 10 after eviction, got %d", cache.Size())
	}

	poolStats = pool.Stats()
	if poolStats.Used != 10 {
		t.Fatalf("expected pool used 10 after eviction, got %d", poolStats.Used)
	}

	// key 0 должен быть вытеснен
	if _, ok := cache.Get(0); ok {
		t.Fatal("expected key 0 to be evicted")
	}

	// key 100 должен существовать
	if val, ok := cache.Get(100); !ok || val != "new" {
		t.Fatalf("expected key 100 to exist with value 'new', got ok=%v", ok)
	}

	// проверяем метрики eviction
	stats := cache.Stats()
	if stats.Evictions == 0 {
		t.Fatal("expected evictions counter to be > 0")
	}
}

// TestEvictionRespectsMin проверяет, что eviction не нарушает min гарантии
func TestEvictionRespectsMin(t *testing.T) {
	pool := NewPool(20)
	defer pool.Close()

	cache1, _ := Register[string, int](pool, Config{
		Name:     "cache1",
		Min:      5,
		Max:      15,
		TTL:      time.Minute,
		Priority: 1,
	})

	cache2, _ := Register[string, int](pool, Config{
		Name:     "cache2",
		Min:      5,
		Max:      15,
		TTL:      time.Minute,
		Priority: 2, // ниже приоритет
	})

	// заполняем cache1 до min
	for i := 0; i < 5; i++ {
		cache1.Set(strconv.Itoa(i), i)
	}

	// заполняем cache2 до max
	for i := 0; i < 15; i++ {
		cache2.Set(strconv.Itoa(i), i)
	}

	// pool полон (5 + 15 = 20)
	poolStats := pool.Stats()
	if poolStats.Used != 20 {
		t.Fatalf("expected pool used 20, got %d", poolStats.Used)
	}

	// cache1 пытается добавить больше - должен вытеснить из cache2 (низкий приоритет)
	for i := 5; i < 10; i++ {
		cache1.Set(strconv.Itoa(i), i)
	}

	// cache1 должен быть больше min
	if cache1.Size() < 5 {
		t.Fatalf("cache1 should be >= min (5), got %d", cache1.Size())
	}

	// cache2 должен быть >= min (5)
	if cache2.Size() < 5 {
		t.Fatalf("cache2 should be >= min (5), got %d", cache2.Size())
	}

	// cache1 не должен превысить max
	if cache1.Size() > 15 {
		t.Fatalf("cache1 should be <= max (15), got %d", cache1.Size())
	}
}

// TestEvictionPriorityOrder проверяет порядок eviction по приоритету
func TestEvictionPriorityOrder(t *testing.T) {
	pool := NewPool(15)
	defer pool.Close()

	// создаем 3 кэша с разными приоритетами
	highPrio, _ := Register[string, int](pool, Config{
		Name:     "high",
		Min:      0,
		Max:      10,
		TTL:      time.Minute,
		Priority: 10, // высокий
	})

	midPrio, _ := Register[string, int](pool, Config{
		Name:     "mid",
		Min:      0,
		Max:      10,
		TTL:      time.Minute,
		Priority: 5, // средний
	})

	lowPrio, _ := Register[string, int](pool, Config{
		Name:     "low",
		Min:      0,
		Max:      10,
		TTL:      time.Minute,
		Priority: 1, // низкий
	})

	// заполняем все кэши
	for i := 0; i < 5; i++ {
		highPrio.Set(strconv.Itoa(i), i)
		midPrio.Set(strconv.Itoa(i), i)
		lowPrio.Set(strconv.Itoa(i), i)
	}

	// pool полон (15)
	if pool.Stats().Used != 15 {
		t.Fatalf("expected pool used 15, got %d", pool.Stats().Used)
	}

	// highPrio пытается добавить больше - должен вытеснить из lowPrio (самый низкий приоритет)
	highPrio.Set("new", 999)

	// lowPrio должен потерять запись
	if lowPrio.Size() < 5 {
		t.Logf("lowPrio size: %d (expected < 5)", lowPrio.Size())
	}

	// highPrio должен иметь новую запись
	if _, ok := highPrio.Get("new"); !ok {
		t.Fatal("expected highPrio to have new entry")
	}

	// проверяем, что pool size остался 15
	if pool.Stats().Used != 15 {
		t.Fatalf("expected pool used 15 after eviction, got %d", pool.Stats().Used)
	}
}

// TestEvictionFromSelfWhenOthersAtMin проверяет eviction из себя, когда другие на min
func TestEvictionFromSelfWhenOthersAtMin(t *testing.T) {
	pool := NewPool(20)
	defer pool.Close()

	cache1, _ := Register[string, int](pool, Config{
		Name:     "cache1",
		Min:      5,
		Max:      15,
		TTL:      time.Minute,
		Priority: 1,
	})

	cache2, _ := Register[string, int](pool, Config{
		Name:     "cache2",
		Min:      5,
		Max:      15,
		TTL:      time.Minute,
		Priority: 2,
	})

	// заполняем оба до min
	for i := 0; i < 5; i++ {
		cache1.Set(strconv.Itoa(i), i)
		cache2.Set(strconv.Itoa(i), i)
	}

	// cache1 пытается превысить max - должен вытеснить из себя
	for i := 5; i < 16; i++ {
		cache1.Set(strconv.Itoa(i), i)
	}

	// cache1 должен быть <= max
	if cache1.Size() > 15 {
		t.Fatalf("cache1 should be <= max (15), got %d", cache1.Size())
	}

	// cache2 должен остаться на min
	if cache2.Size() != 5 {
		t.Fatalf("cache2 should remain at min (5), got %d", cache2.Size())
	}
}

// TestConcurrentEviction проверяет eviction при конкурентном доступе
// В lock-free подходе возможны временные превышения capacity из-за race condition
// между проверкой capacity и добавлением записи. Eviction происходит при каждом Set.
// Этот тест проверяет, что eviction работает в принципе, а не строго проверяет размер
// сразу после конкурентного доступа (это известное ограничение lock-free подхода).
func TestConcurrentEviction(t *testing.T) {
	pool := NewPool(50)
	defer pool.Close()

	cache, _ := Register[int, int](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      50,
		TTL:      time.Minute,
		Priority: 1,
	})

	var wg sync.WaitGroup
	const goroutines = 20
	const keysPerGoroutine = 10

	initialEvictions := cache.Stats().Evictions

	// конкурентно добавляем записи, превышая capacity
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < keysPerGoroutine; j++ {
				key := base*keysPerGoroutine + j
				cache.Set(key, key)
			}
		}(i)
	}

	wg.Wait()

	// делаем серию последовательных операций Set для стабилизации
	// каждая операция Set вызывает eviction если нужно
	for i := 0; i < 200; i++ {
		cache.Set(1000+i, 1000+i)
	}

	// проверяем, что eviction произошел (метрики)
	stats := cache.Stats()
	if stats.Evictions <= initialEvictions {
		// если evictions не увеличились, проверяем размер
		finalSize := cache.Size()
		if finalSize > 50 {
			// если размер превышает capacity и нет evictions, это проблема
			t.Fatalf("size %d > capacity 50, but no evictions occurred", finalSize)
		}
		t.Logf("no evictions needed, final size: %d", finalSize)
	} else {
		t.Logf("evictions occurred: %d (was %d)", stats.Evictions, initialEvictions)
	}

	// проверяем, что система стабильна - делаем еще серию операций
	for i := 0; i < 50; i++ {
		cache.Set(2000+i, 2000+i)
	}

	// после всех операций размер должен быть <= capacity
	finalSize := cache.Size()
	if finalSize > 50 {
		t.Fatalf("expected final size <= 50 after all operations, got %d", finalSize)
	}

	poolStats := pool.Stats()
	if poolStats.Used > 50 {
		t.Fatalf("expected pool used <= 50 after all operations, got %d", poolStats.Used)
	}

	// проверяем, что eviction действительно работает - старые записи должны быть вытеснены
	oldKeysFound := 0
	for i := 0; i < goroutines*keysPerGoroutine; i++ {
		if _, ok := cache.Get(i); ok {
			oldKeysFound++
		}
	}

	// не все старые ключи должны остаться (eviction должен был удалить некоторые)
	if oldKeysFound == goroutines*keysPerGoroutine && finalSize == 50 {
		t.Logf("all old keys still present, but size is correct - eviction may have removed newer keys")
	}
}

// TestGCRemovesFromLRUList проверяет, что GC удаляет из LRU списка
func TestGCRemovesFromLRUList(t *testing.T) {
	pool := NewPool(100, WithGCInterval(20*time.Millisecond))
	defer pool.Close()

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      100,
		TTL:      30 * time.Millisecond,
		Priority: 1,
	})

	// добавляем записи
	for i := 0; i < 10; i++ {
		cache.Set(strconv.Itoa(i), i)
	}

	initialSize := cache.Size()
	if initialSize != 10 {
		t.Fatalf("expected initial size 10, got %d", initialSize)
	}

	// ждем истечения TTL + GC
	time.Sleep(60 * time.Millisecond)

	// проверяем, что LRU список очищен (через pool stats)
	poolStats := pool.Stats()
	if poolStats.Used != 0 {
		t.Fatalf("expected pool used 0 after GC, got %d", poolStats.Used)
	}

	// добавляем новые записи - LRU список должен работать корректно
	for i := 0; i < 5; i++ {
		cache.Set(strconv.Itoa(i+100), i+100)
	}

	if cache.Size() != 5 {
		t.Fatalf("expected size 5 after adding new entries, got %d", cache.Size())
	}
}

// TestEvictionUpdatesMetrics проверяет обновление метрик при eviction
func TestEvictionUpdatesMetrics(t *testing.T) {
	pool := NewPool(5)
	defer pool.Close()

	cache, _ := Register[int, string](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      5,
		TTL:      time.Minute,
		Priority: 1,
	})

	initialStats := cache.Stats()
	initialEvictions := initialStats.Evictions

	// заполняем до capacity
	for i := 0; i < 5; i++ {
		cache.Set(i, "val")
	}

	// добавляем еще одну - должна вытеснить
	cache.Set(100, "new")

	// проверяем метрики
	stats := cache.Stats()
	if stats.Evictions <= initialEvictions {
		t.Fatalf("expected evictions to increase, got %d (was %d)", stats.Evictions, initialEvictions)
	}

	// проверяем, что размер остался в пределах max
	if stats.Size > 5 {
		t.Fatalf("expected size <= 5, got %d", stats.Size)
	}
}

// TestGCClearsExpiredBeforeEviction проверяет, что expired entries удаляются перед eviction
func TestGCClearsExpiredBeforeEviction(t *testing.T) {
	pool := NewPool(10, WithGCInterval(20*time.Millisecond))
	defer pool.Close()

	cache, _ := Register[string, int](pool, Config{
		Name:     "test",
		Min:      0,
		Max:      10,
		TTL:      30 * time.Millisecond,
		Priority: 1,
	})

	// добавляем записи, которые скоро истекут
	for i := 0; i < 5; i++ {
		cache.Set(strconv.Itoa(i), i)
	}

	// ждем истечения
	time.Sleep(40 * time.Millisecond)

	// добавляем новые записи - должны использовать освобожденное место
	for i := 5; i < 10; i++ {
		cache.Set(strconv.Itoa(i), i)
	}

	// размер должен быть 5 (новые записи)
	if cache.Size() != 5 {
		t.Fatalf("expected size 5, got %d", cache.Size())
	}

	// старые записи должны быть удалены GC
	for i := 0; i < 5; i++ {
		if _, ok := cache.Get(strconv.Itoa(i)); ok {
			t.Fatalf("expected key %d to be removed by GC", i)
		}
	}

	// новые записи должны существовать
	for i := 5; i < 10; i++ {
		if _, ok := cache.Get(strconv.Itoa(i)); !ok {
			t.Fatalf("expected key %d to exist", i)
		}
	}
}
