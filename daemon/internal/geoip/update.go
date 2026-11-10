package geoip

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// URLTemplate is where DB-IP publishes the free country database; %s is
// the month ("2026-11"). A new file appears early each month.
const URLTemplate = "https://download.db-ip.com/free/dbip-country-lite-%s.csv.gz"

// MinRanges is the least a real database holds; anything smaller is an
// error page or a truncated download, not a database.
const MinRanges = 100000

// Newest loads whichever of the given files is the most recent database
// (by its recorded month), skipping ones that are missing or unreadable.
func Newest(paths ...string) *DB {
	var best *DB
	for _, p := range paths {
		if p == "" {
			continue
		}
		db, err := Load(p)
		if err != nil {
			continue
		}
		if best == nil || db.Date > best.Date {
			best = db
		}
	}
	return best
}

// Download fetches the database for a month, checks that it really is
// one, stores it at path (with its ".date" companion) and returns it
// loaded. notFound reports that the month has not been published yet.
func Download(ctx context.Context, client *http.Client, urlTemplate, month, path string, minRanges int) (db *DB, notFound bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(urlTemplate, month), nil)
	if err != nil {
		return nil, false, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, true, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("server answered %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, false, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("not a gzip file: %w", err)
	}
	db, err = Parse(gz)
	if err != nil {
		return nil, false, err
	}
	if n := len(db.v4) + len(db.v6); n < minRanges {
		return nil, false, fmt.Errorf("only %d ranges; not a complete database", n)
	}
	db.Date = month
	if err := config.WriteFileAtomic(path, body, 0o644); err != nil {
		return nil, false, err
	}
	if err := config.WriteFileAtomic(path+".date", []byte(month+"\n"), 0o644); err != nil {
		return nil, false, err
	}
	return db, false, nil
}

// Month formats a time as a database month.
func Month(t time.Time) string { return t.UTC().Format("2006-01") }

// Version identifies the database in use: its month, "undated" for one
// without a recorded month, "" when there is none.
func (db *DB) Version() string {
	switch {
	case db == nil:
		return ""
	case db.Date == "":
		return "undated"
	}
	return db.Date
}

// Size reports the number of ranges (for logs and tests).
func (db *DB) Size() int {
	if db == nil {
		return 0
	}
	return len(db.v4) + len(db.v6)
}

// ReadDate returns the month recorded next to a database file.
func ReadDate(path string) string {
	b, _ := os.ReadFile(path + ".date")
	return strings.TrimSpace(string(b))
}
