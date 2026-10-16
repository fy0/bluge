//go:build windows

package bluge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectionPublishTransientSharingConflict(t *testing.T) {
	dir := t.TempDir()
	source, target := filepath.Join(dir, "pending.json"), filepath.Join(dir, "spec.json")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		done <- reader.Close()
	}()
	err = replaceCollectionFile(source, target)
	if closeErr := <-done; closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("replacement = %q, %v", data, err)
	}
}
