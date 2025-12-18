#  pkg

Thread-safe LRU cache pool с поддержкой TTL, приоритетов и общей памяти для Go.

## Особенности

- **Общий пул памяти** — несколько типизированных кешей делят один лимит
- **Приоритетное вытеснение** — низкоприоритетные кеши вытесняются первыми
- **Гарантированный минимум** — каждый кеш защищён от полного вытеснения
- **TTL** — автоматическая инвалидация устаревших записей
- **Singleflight** — защита от thundering herd при загрузке данных
- **Generics** — полная типобезопасность
- **Метрики** — hits, misses, evictions, hit rate
- **Фоновая очистка** — опциональный GC для expired записей

## Установка

```bash
go get github.com/yourname/ pkg
```

## Быстрый старт

```go
package main

import (
	"fmt"
	"time"
	" pkg"
)

type User struct {
	ID   int
	Name string
}

func main() {
	// Создаём пул на 10000 элементов
	pool :=  pkg.NewPool(10000)

	// Регистрируем типизированный кеш
	users :=  pkg.MustRegister[int, User](pool,  pkg.Config{
		Name:     "users",
		Min:      100,   // минимум 100 слотов гарантировано
		Max:      5000,  // максимум 5000 слотов
		TTL:      5 * time.Minute,
		Priority: 10,    // высокий приоритет
	})

	// Базовые операции
	users.Set(1, User{ID: 1, Name: "Alice"})

	if user, ok := users.Get(1); ok {
		fmt.Printf("Found: %+v\n", user)
	}

	// Загрузка с singleflight
	user, err := users.GetOrLoad(2, func() (User, error) {
		// загрузка из БД, вызовется только один раз
		// даже при параллельных запросах
		return User{ID: 2, Name: "Bob"}, nil
	})
}
```

## Конфигурация кеша

```go
 pkg.Config{
Name:     "users",      // уникальное имя для метрик
Min:      100,          // защищённый минимум (не вытесняется другими)
Max:      1000,         // максимальный размер
TTL:      5 * time.Minute, // время жизни записей
Priority: 10,           // приоритет (выше = важнее)
}
```

### Правила валидации

- `sum(min)` всех кешей ≤ `capacity` пула
- `max` ≤ `capacity - sum(min других кешей)`

## Логика вытеснения

При нехватке места в пуле:

1. **Expired** — сначала удаляются просроченные записи из любого кеша
2. **Низкий приоритет** — затем LRU из кеша с наименьшим приоритетом (выше min)
3. **Свой кеш** — если все на минимуме, вытесняем из кеша-инициатора

```
Pool: 100 элементов
├── users    (min=30, priority=10) — вытесняется последним
├── sessions (min=30, priority=5)  — вытесняется вторым  
└── settings (min=20, priority=1)  — вытесняется первым
```

## Фоновая очистка

```go
// С автоматической очисткой expired каждые 30 секунд
pool :=  pkg.NewPoolWithGC(10000, 30*time.Second)
defer pool.Close() // важно для корректного завершения
```

## Метрики

```go
// Статистика конкретного кеша
stats := users.Stats()
fmt.Printf("Size: %d, Hits: %d, Misses: %d, HitRate: %.2f%%\n",
stats.Size, stats.Hits, stats.Misses, stats.HitRate*100)

// Статистика всего пула
poolStats := pool.Stats()
fmt.Printf("Pool: %d/%d used\n", poolStats.Used, poolStats.Capacity)
for name, cs := range poolStats.Caches {
fmt.Printf("  %s: %d items, %.1f%% hit rate\n", name, cs.Size, cs.HitRate*100)
}
```

## API

### Pool

| Метод | Описание |
|-------|----------|
| `NewPool(capacity, ...opts)` | Создать пул (с GC по умолчанию) |
| `WithGCInterval(duration)` | Опция: интервал GC (default: 1m) |
| `WithPromoteBuffer(size)` | Опция: размер буфера promote (default: 1024) |
| `pool.Stats()` | Статистика всех кешей |
| `pool.Close()` | Остановить воркеры |

### Cache[K, V]

| Метод | Описание |
|-------|----------|
| `Register[K, V](pool, config)` | Зарегистрировать кеш |
| `MustRegister[K, V](pool, config)` | То же, но panic при ошибке |
| `cache.Get(key)` | Получить значение |
| `cache.Set(key, value)` | Записать значение |
| `cache.GetOrLoad(key, loader)` | Получить или загрузить (singleflight) |
| `cache.GetOrLoadBatch(batchLoader, cfg)` | Создать batch loader (DataLoader) |
| `cache.Stats()` | Статистика кеша |
| `cache.Size()` | Текущий размер |
| `cache.Name()` | Имя кеша |

## Производительность

```
BenchmarkSet-4              2851964    406 ns/op    32 B/op    3 allocs/op
BenchmarkGet-4              3391302    381 ns/op    15 B/op    1 allocs/op
BenchmarkGetParallel-4      9677872    206 ns/op    15 B/op    1 allocs/op  ← lock-free
BenchmarkSetParallel-4      4323874    337 ns/op    32 B/op    3 allocs/op
```

## Singleflight

`GetOrLoad` реализует per-key singleflight — при параллельных запросах одного ключа loader вызовется только один раз. Другие ключи не блокируются.

```go
// 10 горутин запрашивают ключ 1 → 1 вызов loader
// Параллельно 10 горутин запрашивают ключ 2 → ещё 1 вызов loader
// Итого: 2 вызова, не 20
```

## Batch Loading (DataLoader pattern)

Для оптимизации N+1 запросов — батчинг как в GraphQL DataLoader:

```go
// Создаём batch loader
loadUser := cache.GetOrLoadBatch(
    func(ids []int) (map[int]User, error) {
        // Один SQL запрос вместо N
        return db.GetUsersByIDs(ids)
    },
     pkg.BatchConfig{
        MaxWait:  5 * time.Millisecond, // ждём накопления
        MaxBatch: 100,                   // макс ключей за раз
    },
)

// Использование в резолверах
var wg sync.WaitGroup
for _, order := range orders {
    wg.Add(1)
    go func(userID int) {
        defer wg.Done()
        user, err := loadUser(userID)  // накапливаются в batch
        // ...
    }(order.UserID)
}
wg.Wait()
// 100 заказов → 1-2 SQL запроса вместо 100
```

**Как работает:**
1. Первый запрос запускает таймер (MaxWait)
2. Последующие запросы накапливаются в буфер
3. По таймеру или при достижении MaxBatch — отправляется batch
4. Результаты кешируются и раздаются ожидающим

## Производительность

```go
func main() {
pool :=  pkg.NewPoolWithGC(50000, time.Minute)
defer pool.Close()

// Горячие данные — высокий приоритет
users :=  pkg.MustRegister[int64, User](pool,  pkg.Config{
Name: "users", Min: 5000, Max: 20000, TTL: 10 * time.Minute, Priority: 10,
})

// Сессии — средний приоритет  
sessions :=  pkg.MustRegister[string, Session](pool,  pkg.Config{
Name: "sessions", Min: 2000, Max: 15000, TTL: 30 * time.Minute, Priority: 5,
})

// Справочники — низкий приоритет, долгий TTL
catalogs :=  pkg.MustRegister[string, []Product](pool,  pkg.Config{
Name: "catalogs", Min: 100, Max: 5000, TTL: time.Hour, Priority: 1,
})

// HTTP handlers используют кеши...
}
```

## Лицензия

MIT