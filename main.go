package main

import (
	"cache-pool/model"
	"cache-pool/pkg"
	"time"
)

//TIP <p>To run your code, right-click the code and select <b>Run</b>.</p> <p>Alternatively, click
// the <icon src="AllIcons.Actions.Execute"/> icon in the gutter and select the <b>Run</b> menu item from here.</p>

func main() {
	pool := pkg.NewPoolWithGC(50000, time.Minute)
	defer pool.Close()

	// Горячие данные — высокий приоритет
	users := pkg.MustRegister[int64, model.User](pool, pkg.Config{
		Name: "users", Min: 5000, Max: 20000, TTL: 10 * time.Minute, Priority: 10,
	})

	// Сессии — средний приоритет
	sessions := pkg.MustRegister[string, model.Session](pool, pkg.Config{
		Name: "sessions", Min: 2000, Max: 15000, TTL: 30 * time.Minute, Priority: 5,
	})

	// Справочники — низкий приоритет, долгий TTL
	catalogs := pkg.MustRegister[string, []model.Product](pool, pkg.Config{
		Name: "catalogs", Min: 100, Max: 5000, TTL: time.Hour, Priority: 1,
	})

	// HTTP handlers используют кеши...

	_ = users
	_ = sessions
	_ = catalogs
}
