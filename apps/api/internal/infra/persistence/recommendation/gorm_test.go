package infrarecommendation

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 使用专用连接上的 TEMPORARY TABLE，避免改动已有业务表。
func TestPublishedCandidatesFiltersUnavailableVideosAndAuthors(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN not set, skip MySQL integration test")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ParseTime = true
	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, query := range []string{
		"CREATE TEMPORARY TABLE account (id BIGINT PRIMARY KEY, status INT NOT NULL)",
		"CREATE TEMPORARY TABLE video (id BIGINT PRIMARY KEY, author_id BIGINT NOT NULL, status INT NOT NULL, published_at DATETIME NULL)",
		"INSERT INTO account (id, status) VALUES (10,1),(20,0)",
		"INSERT INTO video (id,author_id,status,published_at) VALUES (1,10,2,'2026-01-01'),(2,10,3,'2026-11-25'),(3,20,2,'2026-11-25'),(4,30,2,'2026-11-25'),(5,10,2,NULL),(6,10,2,'2026-11-25')",
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	gormDB, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := New(gormDB)
	items, err := repo.PublishedCandidates(ctx, []int64{1, 2, 3, 4, 5, 6, 404, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1] == nil || items[6] == nil {
		t.Fatalf("unexpected available candidates: %+v", items)
	}
	if items[1].AuthorID != 10 || items[1].PublishedAt.Year() != 2026 || items[1].PublishedAt.Month() != time.January {
		t.Fatalf("older published item incorrectly excluded: %+v", items[1])
	}
	for _, item := range items {
		if item.HotScore != 0 || item.FreshnessScore != 0 || item.RankScore != 0 {
			t.Fatalf("database computed recommendation score: %+v", item)
		}
	}
	empty, err := repo.PublishedCandidates(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("unexpected empty batch: %+v, %v", empty, err)
	}
}
