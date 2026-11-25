package infraexposure

import (
	domainexposure "GCFeed/internal/domain/exposure"
	domainvideo "GCFeed/internal/domain/video"
	infravideo "GCFeed/internal/infra/persistence/video"
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// GCFEED_TEST_MYSQL_DSN 显式启用真实 MySQL 验证，测试只写临时数据库并在结束后删除。
func viewEventTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("GCFEED_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GCFEED_TEST_MYSQL_DSN not set, skip MySQL persistence test")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	cfg.ParseTime = true
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("gcfeed_view_events_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Errorf("drop temporary database: %v", err)
		}
	})
	cfg.DBName = name
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&infravideo.VideoModel{}, &ViewEventModel{}, &ExposureModel{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1001, 1002} {
		if err := db.Create(&infravideo.VideoModel{ID: id, AuthorID: 1, Status: domainvideo.StatusPublished}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestViewEventConcurrentRetriesAreIdempotent(t *testing.T) {
	db := viewEventTestDB(t)
	repo := New(db)
	ctx := context.Background()
	for _, eventType := range []string{"exposed", "play", "complete"} {
		results := make(chan *domainexposure.ViewEvent, 12)
		errors := make(chan error, 12)
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				event, _ := domainexposure.NewViewEvent(42, 1001, "recommend", "request-1", eventType, 5000, false)
				saved, _, err := repo.SaveViewEvent(ctx, event)
				if err != nil {
					errors <- err
					return
				}
				results <- saved
			}()
		}
		wg.Wait()
		close(results)
		close(errors)
		for err := range errors {
			t.Fatal(err)
		}
		var first *domainexposure.ViewEvent
		for saved := range results {
			if first == nil {
				first = saved
			} else if saved.ID != first.ID || !saved.CreatedAt.Equal(first.CreatedAt) {
				t.Fatalf("%s retries returned different events: first=%+v retry=%+v", eventType, first, saved)
			}
		}
	}
	var count int64
	if err := db.Model(&ViewEventModel{}).Count(&count).Error; err != nil || count != 3 {
		t.Fatalf("expected three training events, count=%d err=%v", count, err)
	}
	var exposure ExposureModel
	if err := db.Where("user_id = ? AND video_id = ?", 42, 1001).Take(&exposure).Error; err != nil || exposure.ExposureCount != 1 {
		t.Fatalf("duplicate exposure was counted, exposure=%+v err=%v", exposure, err)
	}
}

func TestViewEventDeduplicationDimensions(t *testing.T) {
	base, _ := domainexposure.NewViewEvent(42, 1001, "recommend", "request-1", "play", 3000, false)
	key := viewEventDedupKey(base)
	if key == nil || len(*key) != 64 {
		t.Fatal("request ID did not produce a bounded deduplication key")
	}
	for name, change := range map[string]func(*domainexposure.ViewEvent){
		"user":    func(event *domainexposure.ViewEvent) { event.UserID++ },
		"video":   func(event *domainexposure.ViewEvent) { event.VideoID++ },
		"scene":   func(event *domainexposure.ViewEvent) { event.Scene = "timeline" },
		"request": func(event *domainexposure.ViewEvent) { event.RequestID = "request-2" },
		"type":    func(event *domainexposure.ViewEvent) { event.EventType = "complete" },
	} {
		copy := *base
		change(&copy)
		if got := viewEventDedupKey(&copy); got == nil || *got == *key {
			t.Fatalf("distinct %s was deduplicated", name)
		}
	}
	base.RequestID = ""
	if viewEventDedupKey(base) != nil {
		t.Fatal("legacy events without request IDs must remain independent")
	}
}
