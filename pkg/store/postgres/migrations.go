package postgres

import (
	"embed"
	"fmt"
	"sort"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Migration struct {
	Name string
	SQL  string
}

func Migrations() ([]Migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	sort.Slice(entries, func(leftIndex, rightIndex int) bool {
		return entries[leftIndex].Name() < entries[rightIndex].Name()
	})

	migrations := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := "migrations/" + entry.Name()
		content, err := migrationFiles.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read embedded migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, Migration{Name: entry.Name(), SQL: string(content)})
	}
	return migrations, nil
}
