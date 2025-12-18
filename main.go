package main

import (
	"cache-pool/pkg"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

// User представляет данные пользователя
type User struct {
	ID    int
	Name  string
	Email string
}

// Session представляет сессию пользователя
type Session struct {
	Token     string
	UserID    int
	CreatedAt time.Time
}

func main() {
	// Создаём пул с общим лимитом 10000 элементов и фоновой очисткой каждые 30 секунд
	pool := pkg.NewPool(10000, pkg.WithGCInterval(30*time.Second))
	defer pool.Close()

	// Регистрируем кеш пользователей
	// Высокий приоритет (10), минимум 1000 слотов гарантировано
	users := pkg.MustRegister[int, User](pool, pkg.Config{
		Name:     "users",
		Min:      1000,
		Max:      5000,
		TTL:      10 * time.Minute,
		Priority: 10,
	})

	// Регистрируем кеш сессий
	// Средний приоритет (5), короткий TTL
	sessions := pkg.MustRegister[string, Session](pool, pkg.Config{
		Name:     "sessions",
		Min:      500,
		Max:      3000,
		TTL:      5 * time.Minute,
		Priority: 5,
	})

	// Регистрируем кеш настроек
	// Низкий приоритет (1), будет вытесняться первым при нехватке памяти
	settings := pkg.MustRegister[string, string](pool, pkg.Config{
		Name:     "settings",
		Min:      100,
		Max:      2000,
		TTL:      1 * time.Hour,
		Priority: 1,
	})

	fmt.Println("=== LRU Cache Pool Demo ===\n")

	// Демо: базовые операции
	fmt.Println("1. Базовые операции Set/Get:")
	users.Set(1, User{ID: 1, Name: "Alice", Email: "alice@example.com"})
	users.Set(2, User{ID: 2, Name: "Bob", Email: "bob@example.com"})

	if user, ok := users.Get(1); ok {
		fmt.Printf("   Найден пользователь: %+v\n", user)
	}

	// Демо: GetOrLoad с автозагрузкой
	fmt.Println("\n2. GetOrLoad (загрузка при отсутствии):")
	user, err := users.GetOrLoad(3, func() (User, error) {
		fmt.Println("   [loader] Загружаем пользователя 3 из БД...")
		time.Sleep(10 * time.Millisecond) // имитация запроса к БД
		return User{ID: 3, Name: "Charlie", Email: "charlie@example.com"}, nil
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   Загружен: %+v\n", user)

	// Повторный вызов — из кеша
	user, _ = users.GetOrLoad(3, func() (User, error) {
		fmt.Println("   [loader] Этот текст не должен появиться!")
		return User{}, nil
	})
	fmt.Printf("   Из кеша: %+v\n", user)

	// Демо: singleflight
	fmt.Println("\n3. Singleflight (10 горутин, 1 загрузка):")
	var wg sync.WaitGroup
	var loadCount int
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			users.GetOrLoad(100, func() (User, error) {
				mu.Lock()
				loadCount++
				mu.Unlock()
				time.Sleep(50 * time.Millisecond)
				return User{ID: 100, Name: "Concurrent", Email: "concurrent@example.com"}, nil
			})
		}()
	}
	wg.Wait()
	fmt.Printf("   Загрузок выполнено: %d (ожидалось 1)\n", loadCount)

	// Демо: batch loading (DataLoader pattern)
	fmt.Println("\n4. Batch Loading (DataLoader):")
	var batchCalls int
	loadUserBatch := users.GetOrLoadBatch(
		func(ids []int) (map[int]User, error) {
			batchCalls++
			fmt.Printf("   [batch] Загружаем %d пользователей одним запросом: %v\n", len(ids), ids)
			time.Sleep(10 * time.Millisecond) // имитация batch SQL
			result := make(map[int]User)
			for _, id := range ids {
				result[id] = User{ID: id, Name: fmt.Sprintf("BatchUser%d", id)}
			}
			return result, nil
		},
		pkg.BatchConfig{
			MaxWait:  10 * time.Millisecond,
			MaxBatch: 50,
		},
	)

	// 20 горутин запрашивают разных пользователей → батчатся
	var wg2 sync.WaitGroup
	for i := 200; i < 220; i++ {
		wg2.Add(1)
		go func(id int) {
			defer wg2.Done()
			loadUserBatch(id)
		}(i)
	}
	wg2.Wait()
	fmt.Printf("   Batch вызовов: %d (для 20 ключей)\n", batchCalls)

	// Демо: разные кеши
	fmt.Println("\n5. Работа с разными кешами:")
	sessions.Set("token-abc", Session{Token: "token-abc", UserID: 1, CreatedAt: time.Now()})
	settings.Set("theme", "dark")
	settings.Set("language", "ru")

	if sess, ok := sessions.Get("token-abc"); ok {
		fmt.Printf("   Сессия: UserID=%d\n", sess.UserID)
	}
	if theme, ok := settings.Get("theme"); ok {
		fmt.Printf("   Тема: %s\n", theme)
	}

	// Демо: нагрузка и вытеснение
	fmt.Println("\n6. Симуляция нагрузки (5000 записей):")
	for i := 0; i < 5000; i++ {
		users.Set(i, User{ID: i, Name: fmt.Sprintf("User%d", i)})
	}

	// Статистика
	fmt.Println("\n7. Статистика:")
	printStats(pool)

	// Демо: приоритетное вытеснение
	fmt.Println("\n8. Приоритетное вытеснение:")
	fmt.Println("   Добавляем ещё 3000 пользователей (высокий приоритет)...")
	for i := 5000; i < 8000; i++ {
		users.Set(i, User{ID: i, Name: fmt.Sprintf("User%d", i)})
	}
	printStats(pool)
	fmt.Println("   → settings (приоритет 1) вытесняется первым")

	// Бенчмарк
	fmt.Println("\n9. Быстрый бенчмарк:")
	benchmark(users)
}

func printStats(pool *pkg.Pool) {
	stats := pool.Stats()
	fmt.Printf("   Pool: %d/%d used, %d free\n", stats.Used, stats.Capacity, stats.Free)
	for name, cs := range stats.Caches {
		fmt.Printf("   - %s: size=%d (min=%d, max=%d), hits=%d, misses=%d, hit_rate=%.1f%%, evictions=%d\n",
			name, cs.Size, cs.Min, cs.Max, cs.Hits, cs.Misses, cs.HitRate*100, cs.Evictions)
	}
}

func benchmark(cache *pkg.Cache[int, User]) {
	const ops = 100000

	// Set benchmark
	start := time.Now()
	for i := 0; i < ops; i++ {
		cache.Set(rand.Intn(10000), User{ID: i})
	}
	setDur := time.Since(start)

	// Get benchmark
	start = time.Now()
	for i := 0; i < ops; i++ {
		cache.Get(rand.Intn(10000))
	}
	getDur := time.Since(start)

	fmt.Printf("   Set: %d ops in %v (%.0f ns/op)\n", ops, setDur, float64(setDur.Nanoseconds())/float64(ops))
	fmt.Printf("   Get: %d ops in %v (%.0f ns/op)\n", ops, getDur, float64(getDur.Nanoseconds())/float64(ops))
}
