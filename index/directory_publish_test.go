package index

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/fy0/bluge/segment"
)

type directoryWriteFunc func(io.Writer) (int64, error)

func (f directoryWriteFunc) WriteTo(w io.Writer, _ chan struct{}) (int64, error) {
	return f(w)
}

func TestFileSystemDirectoryAtomicPublish(t *testing.T) {
	for _, kind := range []string{ItemKindSegment, ItemKindSnapshot} {
		t.Run(kind, func(t *testing.T) {
			directory := NewFileSystemDirectory(t.TempDir())
			directory.newFilePerm = 0o640
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			done := make(chan error, 1)
			go func() {
				done <- directory.Persist(kind, 7, directoryWriteFunc(func(w io.Writer) (int64, error) {
					n, err := io.WriteString(w, "partial")
					if err != nil {
						return int64(n), err
					}
					close(started)
					<-release
					m, err := io.WriteString(w, "complete")
					return int64(n + m), err
				}), nil)
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("write did not start: %v", err)
			}
			ids, listErr := directory.List(kind)
			path := filepath.Join(directory.path, directory.fileName(kind, 7))
			_, statErr := os.Stat(path)
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if listErr != nil || len(ids) != 0 || !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("uncommitted file visible: ids=%v list=%v stat=%v", ids, listErr, statErr)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "partialcomplete" {
				t.Fatalf("published content: %q, %v", data, err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != directory.newFilePerm {
				t.Fatalf("mode=%v want=%v", info.Mode().Perm(), directory.newFilePerm)
			}
		})
	}
}

func TestFileSystemDirectoryFailedPublishCleansTemporaryFile(t *testing.T) {
	directory := NewFileSystemDirectory(t.TempDir())
	failure := errors.New("write failed")
	err := directory.Persist(ItemKindSnapshot, 7, directoryWriteFunc(func(w io.Writer) (int64, error) {
		n, err := io.WriteString(w, "partial")
		return int64(n), errors.Join(err, failure)
	}), nil)
	if !errors.Is(err, failure) {
		t.Fatalf("write error: %v", err)
	}
	entries, err := os.ReadDir(directory.path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed write left files: %v, %v", entries, err)
	}
}

type segmentLoadHookDirectory struct {
	Directory
	beforeSegment func()
}

func (d *segmentLoadHookDirectory) Load(kind string, id uint64) (*segment.Data, io.Closer, error) {
	if kind == ItemKindSegment {
		d.beforeSegment()
	}
	return d.Directory.Load(kind, id)
}

func TestOpenReaderPinsSnapshotUntilSegmentsAreLoaded(t *testing.T) {
	config := DefaultConfig(t.TempDir())
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	updateOneDoc(t, writer, "1")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	directory := config.DirectoryFunc()
	epochs, err := directory.List(ItemKindSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	config.DirectoryFunc = func() Directory {
		return &segmentLoadHookDirectory{Directory: directory, beforeSegment: func() {
			checked = true
			if err := directory.Remove(ItemKindSnapshot, epochs[0]); err == nil {
				t.Error("snapshot removed before its segments were pinned")
			}
		}}
	}
	reader, err := OpenReader(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if !checked {
		t.Fatal("no segment was loaded")
	}
}

type snapshotListHookDirectory struct {
	Directory
	afterList func() error
}

func (d *snapshotListHookDirectory) List(kind string) ([]uint64, error) {
	ids, err := d.Directory.List(kind)
	if err == nil && kind == ItemKindSnapshot && d.afterList != nil {
		hook := d.afterList
		d.afterList = nil
		err = hook()
	}
	return ids, err
}

func TestOpenReaderFollowsPublishedSuccessor(t *testing.T) {
	config := DefaultConfig(t.TempDir())
	directory := config.DirectoryFunc()
	if err := directory.Persist(ItemKindSnapshot, 1, &Snapshot{epoch: 1}, nil); err != nil {
		t.Fatal(err)
	}
	wrapped := &snapshotListHookDirectory{Directory: directory, afterList: func() error {
		if err := directory.Persist(ItemKindSnapshot, 2, &Snapshot{epoch: 2}, nil); err != nil {
			return err
		}
		return directory.Remove(ItemKindSnapshot, 1)
	}}
	config.DirectoryFunc = func() Directory { return wrapped }
	reader, err := OpenReader(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.epoch != 2 {
		t.Fatalf("epoch=%d want=2", reader.epoch)
	}
}

func TestOpenReaderRejectsCorruptLatestSnapshot(t *testing.T) {
	config := DefaultConfig(t.TempDir())
	directory := config.DirectoryFunc()
	if err := directory.Persist(ItemKindSnapshot, 1, &Snapshot{epoch: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := directory.Persist(ItemKindSnapshot, 2, directoryWriteFunc(func(w io.Writer) (int64, error) {
		n, err := io.WriteString(w, "invalid")
		return int64(n), err
	}), nil); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(config)
	if err == nil {
		_ = reader.Close()
		t.Fatal("corrupt latest snapshot was accepted")
	}
}
