package packstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRetainsEveryPortAndIndexesLatestByPackID(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "cone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := store.Save(context.Background(), Record{
		PackID: "abc123", PackName: "first.zip", OutputJSON: []byte(`{"version":1}`),
		CreatedAt: time.Unix(100, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(context.Background(), Record{
		PackID: "abc123", PackName: "second.zip", OutputJSON: []byte(`{"version":2}`),
		CreatedAt: time.Unix(200, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("sequences = %d, %d; want 1, 2", first.Sequence, second.Sequence)
	}
	count, err := store.Count(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("count = %d, %v; want 2", count, err)
	}
	latest, found, err := store.Latest(context.Background(), "abc123")
	if err != nil || !found || latest.Sequence != second.Sequence || latest.PackName != "second.zip" {
		t.Fatalf("latest = %#v, found=%v, err=%v", latest, found, err)
	}
}

func TestStoreRejectsInvalidRecords(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "cone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Save(context.Background(), Record{PackID: "id", PackName: "pack.zip", OutputJSON: []byte("not json")}); err == nil {
		t.Fatal("invalid output JSON was accepted")
	}
}

func TestStoreListReturnsNewestFirstAndGetFindsBySequence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "cone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := store.Save(context.Background(), Record{
		PackID: "aaa", PackName: "first.zip", OutputJSON: []byte(`{"a":1}`),
		PreviewPNG: []byte("first-preview"), CreatedAt: time.Unix(100, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(context.Background(), Record{
		PackID: "bbb", PackName: "second.zip", OutputJSON: []byte(`{"a":2}`),
		CreatedAt: time.Unix(200, 0),
	})
	if err != nil {
		t.Fatal(err)
	}

	list, err := store.List(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Sequence != second.Sequence || list[1].Sequence != first.Sequence {
		t.Fatalf("List() = %#v, want [second, first]", list)
	}

	fetched, found, err := store.Get(context.Background(), first.Sequence)
	if err != nil || !found || fetched.PackName != "first.zip" || string(fetched.PreviewPNG) != "first-preview" {
		t.Fatalf("Get(first) = %#v, found=%v, err=%v", fetched, found, err)
	}

	limited, err := store.List(context.Background(), 1)
	if err != nil || len(limited) != 1 || limited[0].Sequence != second.Sequence {
		t.Fatalf("List(limit=1) = %#v, err=%v; want only the newest record", limited, err)
	}

	_, found, err = store.Get(context.Background(), 9999)
	if err != nil || found {
		t.Fatalf("Get(missing) found=%v, err=%v; want not found", found, err)
	}
}
