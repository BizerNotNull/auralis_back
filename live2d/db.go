package live2d

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openDatabaseFromEnv() (*gorm.DB, error) {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_DSN"))
	if dsn == "" {
		return nil, errors.New("live2d: DATABASE_DSN environment variable is required")
	}

	driver := strings.TrimSpace(os.Getenv("DATABASE_DRIVER"))
	if driver == "" {
		driver = inferDriverFromDSN(dsn)
		if driver == "" {
			return nil, errors.New("live2d: DATABASE_DRIVER environment variable is required when DSN does not contain a scheme")
		}
	}

	return openDatabase(driver, dsn)
}

func openDatabase(driver, dsn string) (*gorm.DB, error) {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pg":
		return gorm.Open(postgres.Open(dsn), &gorm.Config{NowFunc: func() time.Time { return time.Now().UTC() }})
	case "mysql":
		return gorm.Open(mysql.Open(dsn), &gorm.Config{NowFunc: func() time.Time { return time.Now().UTC() }})
	case "sqlite", "sqlite3":
		return gorm.Open(sqlite.Open(dsn), &gorm.Config{NowFunc: func() time.Time { return time.Now().UTC() }})
	default:
		return nil, fmt.Errorf("live2d: unsupported database driver %q", driver)
	}
}

func inferDriverFromDSN(dsn string) string {
	lower := strings.ToLower(dsn)
	switch {
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		return "postgres"
	case strings.HasPrefix(lower, "mysql://"), strings.Contains(lower, "://mysql"):
		return "mysql"
	case strings.HasPrefix(lower, "sqlite://"), strings.HasSuffix(lower, ".db"), strings.HasSuffix(lower, ".sqlite"):
		return "sqlite"
	default:
		return ""
	}
}
