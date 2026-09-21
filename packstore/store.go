// Package packstore provides Cone's durable, reusable port-history database.
package packstore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const maxOutputJSONBytes = 1 << 20

var (
	portsBucket  = []byte("ports")
	latestBucket = []byte("latest_by_pack")
)

// Record is one successful Cone port. PackID identifies the source ZIP while
// Sequence identifies this specific port, so repeated ports are never lost.
type Record struct {
	Sequence   uint64          `json:"sequence"`
	PackID     string          `json:"packId"`
	PackName   string          `json:"packName"`
	OutputJSON json.RawMessage `json:"outputJson"`
	PreviewPNG []byte          `json:"previewPng,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// Store is safe for concurrent readers and writers.
type Store struct {
	db   *bolt.DB
	path string
}

// Open creates or opens a Cone port database at path.
func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("pack database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create pack database directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open pack database: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(portsBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(latestBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize pack database: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

func (store *Store) Path() string {
	if store == nil {
		return ""
	}
	return store.path
}

func (store *Store) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

// Save appends a successful port and updates the latest record for its PackID.
func (store *Store) Save(ctx context.Context, record Record) (Record, error) {
	if store == nil || store.db == nil {
		return Record{}, errors.New("pack database is closed")
	}
	if ctx == nil {
		return Record{}, errors.New("pack database context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	record.PackID = strings.TrimSpace(record.PackID)
	record.PackName = strings.TrimSpace(record.PackName)
	if record.PackID == "" || record.PackName == "" {
		return Record{}, errors.New("pack ID and name are required")
	}
	if len(record.OutputJSON) == 0 || len(record.OutputJSON) > maxOutputJSONBytes || !json.Valid(record.OutputJSON) {
		return Record{}, errors.New("pack output JSON is empty, invalid, or too large")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	} else {
		record.CreatedAt = record.CreatedAt.UTC()
	}
	record.OutputJSON = append(json.RawMessage(nil), record.OutputJSON...)
	if record.PreviewPNG != nil {
		record.PreviewPNG = append([]byte(nil), record.PreviewPNG...)
	}
	err := store.db.Update(func(tx *bolt.Tx) error {
		ports := tx.Bucket(portsBucket)
		latest := tx.Bucket(latestBucket)
		sequence, err := ports.NextSequence()
		if err != nil {
			return err
		}
		record.Sequence = sequence
		key := sequenceKey(sequence)
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := ports.Put(key, encoded); err != nil {
			return err
		}
		return latest.Put([]byte(record.PackID), key)
	})
	if err != nil {
		return Record{}, fmt.Errorf("save pack record: %w", err)
	}
	return record, nil
}

// Latest returns the newest port for a content-addressed PackID.
func (store *Store) Latest(ctx context.Context, packID string) (Record, bool, error) {
	if store == nil || store.db == nil {
		return Record{}, false, errors.New("pack database is closed")
	}
	if ctx == nil {
		return Record{}, false, errors.New("pack database context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	packID = strings.TrimSpace(packID)
	var record Record
	found := false
	err := store.db.View(func(tx *bolt.Tx) error {
		key := tx.Bucket(latestBucket).Get([]byte(packID))
		if key == nil {
			return nil
		}
		value := tx.Bucket(portsBucket).Get(key)
		if value == nil {
			return errors.New("latest pack index points to a missing record")
		}
		if err := json.Unmarshal(value, &record); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return Record{}, false, fmt.Errorf("read latest pack record: %w", err)
	}
	return record, found, nil
}

// Count returns the number of successful ports, including repeated PackIDs.
func (store *Store) Count(ctx context.Context) (int, error) {
	if store == nil || store.db == nil {
		return 0, errors.New("pack database is closed")
	}
	if ctx == nil {
		return 0, errors.New("pack database context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count := 0
	err := store.db.View(func(tx *bolt.Tx) error {
		count = tx.Bucket(portsBucket).Stats().KeyN
		return nil
	})
	return count, err
}

// defaultLibraryLimit and maxLibraryLimit bound how many records List will
// ever return in one call, so a growing port-history database can never turn
// a single Library page load into an unbounded scan or response body.
const (
	defaultLibraryLimit = 500
	maxLibraryLimit     = 2000
)

// List returns the most recent successful ports, newest first. A limit <= 0
// uses defaultLibraryLimit; limits above maxLibraryLimit are clamped.
func (store *Store) List(ctx context.Context, limit int) ([]Record, error) {
	if store == nil || store.db == nil {
		return nil, errors.New("pack database is closed")
	}
	if ctx == nil {
		return nil, errors.New("pack database context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultLibraryLimit
	}
	if limit > maxLibraryLimit {
		limit = maxLibraryLimit
	}
	records := make([]Record, 0, limit)
	err := store.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(portsBucket).Cursor()
		for key, value := cursor.Last(); key != nil && len(records) < limit; key, value = cursor.Prev() {
			var record Record
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			records = append(records, record)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list pack records: %w", err)
	}
	return records, nil
}

// Get returns a single port by its Sequence number.
func (store *Store) Get(ctx context.Context, sequence uint64) (Record, bool, error) {
	if store == nil || store.db == nil {
		return Record{}, false, errors.New("pack database is closed")
	}
	if ctx == nil {
		return Record{}, false, errors.New("pack database context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	var record Record
	found := false
	err := store.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(portsBucket).Get(sequenceKey(sequence))
		if value == nil {
			return nil
		}
		if err := json.Unmarshal(value, &record); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return Record{}, false, fmt.Errorf("read pack record: %w", err)
	}
	return record, found, nil
}

func sequenceKey(sequence uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, sequence)
	return key
}
