package bluge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fy0/bluge/index"
)

func TestCollectionRevisionMetadataSnapshot(t *testing.T) {
	c, specs, _ := gcCollection(t, "flat")
	old, err := c.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	revisions, err := c.UpsertDocuments([]*Document{collectionTestDoc("a", "updated")})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := OpenCollectionReader(c.config)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if old.revisions != nil || latest.revisions != nil {
		t.Fatal("opening readers loaded document revisions")
	}
	pending, err := latest.PendingVectors(context.Background(), specs[0].ID, "", 10)
	if err != nil || !reflect.DeepEqual(pending, revisions) {
		t.Fatalf("pending revisions: %v, %v", pending, err)
	}
	gcQuery(t, old, specs)
	iterator, err := latest.Search(context.Background(), NewAllMatches(NewTermQuery("updated").SetField("body")))
	if err != nil {
		t.Fatal(err)
	}
	match, err := iterator.Next()
	if err != nil || match == nil {
		t.Fatalf("text result: %v, %v", match, err)
	}
	var storedRevision string
	if err := match.VisitStoredFields(func(name string, value []byte) bool {
		if name == collectionRevisionField {
			storedRevision = string(value)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if storedRevision != revisions[0].Revision {
		t.Fatalf("stored revision: %q, want %q", storedRevision, revisions[0].Revision)
	}
}

func TestCollectionTextWritesKeepManifestStatic(t *testing.T) {
	c, specs, _ := gcCollection(t, "flat")
	manifestPath := filepath.Join(c.config.Path, "collection.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(before, &manifest); err != nil || len(manifest) != 3 || manifest["revisions"] != nil {
		t.Fatalf("manifest: %s, %v", before, err)
	}
	gate, err := lockCollectionCoordinator(context.Background(), c.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	doc := collectionTestDoc("a", "changed")
	fieldCount := len(*doc)
	done := make(chan error, 1)
	go func() {
		if _, err := c.UpsertDocuments([]*Document{doc}, "changed-input"); err != nil {
			done <- err
			return
		}
		done <- c.DeleteDocuments("b")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		_ = gate.Close()
		<-done
		t.Fatal("text writes waited for the generation coordinator")
	}
	if len(*doc) != fieldCount {
		t.Fatal("collection mutated the input document fields")
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("text writes changed the manifest: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCollection(c.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.revisions != nil {
		t.Fatal("opening the writer loaded document revisions")
	}
	r, err := reopened.Reader(specs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	count, err := r.Count()
	if err != nil || count != 1 || r.revisions != nil {
		t.Fatalf("text reader state: count=%d revisions=%v err=%v", count, r.revisions, err)
	}
	iterator, err := r.Search(context.Background(), NewAllMatches(NewTermQuery("changed").SetField("body")))
	if err != nil {
		t.Fatal(err)
	}
	match, err := iterator.Next()
	if err != nil || match == nil || r.revisions != nil {
		t.Fatalf("text search loaded revisions: match=%v revisions=%v err=%v", match, r.revisions, err)
	}
	pending, err := r.PendingVectors(context.Background(), specs[0].ID, "", 10)
	want := []DocumentRevision{{ID: "a", Revision: "changed-input"}}
	if err != nil || !reflect.DeepEqual(pending, want) {
		t.Fatalf("persisted revisions: %v, %v", pending, err)
	}
}

type collectionBlockedSnapshotDirectory struct {
	index.Directory
	armed   atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (d *collectionBlockedSnapshotDirectory) Persist(kind string, id uint64, w index.WriterTo, closeCh chan struct{}) error {
	if kind == index.ItemKindSnapshot && d.armed.Swap(false) {
		close(d.started)
		<-d.release
	}
	return d.Directory.Persist(kind, id, w, closeCh)
}

func TestCollectionTextPersistenceDoesNotBlockVectorReaders(t *testing.T) {
	c, specs, _ := gcCollection(t, "flat")
	if err := c.writer.Close(); err != nil {
		t.Fatal(err)
	}
	config := c.config.TextConfig
	directory := &collectionBlockedSnapshotDirectory{
		Directory: config.indexConfig.DirectoryFunc(), started: make(chan struct{}), release: make(chan struct{}),
	}
	var once sync.Once
	release := func() { once.Do(func() { close(directory.release) }) }
	defer release()
	config.indexConfig.DirectoryFunc = func() index.Directory { return directory }
	var err error
	c.writer, err = OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	old, err := c.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	directory.armed.Store(true)
	written := make(chan error, 1)
	go func() {
		_, err := c.UpsertDocuments([]*Document{collectionTestDoc("a", "changed")}, "changed-input")
		written <- err
	}()
	select {
	case <-directory.started:
	case err := <-written:
		t.Fatalf("write did not reach persistence: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checked := make(chan error, 1)
	go func() {
		checked <- collectionReadDuringTextPersistence(ctx, c, old, specs[0].ID)
	}()
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		release()
		<-checked
		t.Fatal("readers or maintenance waited for text persistence")
	}
	release()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

func collectionReadDuringTextPersistence(ctx context.Context, c *Collection, old *CollectionReader, spaceID string) error {
	independent, err := OpenCollectionReader(c.config)
	if err != nil {
		return err
	}
	defer independent.Close()
	for _, r := range []*CollectionReader{old, independent} {
		iterator, err := r.Search(ctx, NewAllMatches(NewTermQuery("apple").SetField("body")))
		if err != nil {
			return err
		}
		match, err := iterator.Next()
		if err != nil || match == nil {
			return fmt.Errorf("previous text snapshot: match=%v err=%v", match, err)
		}
		status, err := r.VectorStatus(ctx, spaceID)
		if err != nil || status.ValidVectors != 2 || status.PendingVectors != 0 {
			return fmt.Errorf("previous vector snapshot: status=%+v err=%v", status, err)
		}
	}
	_, err = maintainCollection(ctx, c.config, c.corpus)
	return err
}

func TestCollectionRejectsReservedRevisionField(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	docs := []*Document{
		collectionTestDoc("a", "changed"),
		collectionTestDoc("b", "changed").AddField(NewStoredOnlyField(collectionRevisionField, []byte("forged"))),
	}
	if _, err := c.UpsertDocuments(docs); err == nil {
		t.Fatal("accepted reserved revision field")
	}
	r, err := c.Reader(specs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	status, err := r.VectorStatus(context.Background(), specs[0].ID)
	if err != nil || status.ValidVectors != len(revs) || status.PendingVectors != 0 {
		t.Fatalf("invalid batch changed documents: %+v, %v", status, err)
	}
}

func TestCollectionStatusDoesNotReadVectorPayload(t *testing.T) {
	c, specs, _ := gcCollection(t, "flat")
	space := c.spaces[specs[0].ID]
	indexPath, _ := collectionGenerationFiles(collectionSpacePath(c.config.Path, space.Spec.ID), space.Generation)
	info, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, make([]byte, info.Size()), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenCollectionReader(c.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	status, err := reader.VectorStatus(context.Background(), space.Spec.ID)
	if err != nil || status.ValidVectors != 2 {
		t.Fatalf("metadata status: %+v, %v", status, err)
	}
	pending, err := reader.PendingVectors(context.Background(), space.Spec.ID, "", 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("metadata pagination: %v, %v", pending, err)
	}
}
