package remote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type archiveSizeWriter int64

func (w *archiveSizeWriter) Write(data []byte) (int, error) {
	*w += archiveSizeWriter(len(data))
	return len(data), nil
}

func BenchmarkArchiveSmallFiles(b *testing.B) {
	root := b.TempDir()
	if err := os.Mkdir(filepath.Join(root, "files"), 0700); err != nil {
		b.Fatal(err)
	}
	data := make([]byte, 64)
	mtime := time.Unix(1700000000, 123456789)
	for i := 0; i < 10000; i++ {
		path := filepath.Join(root, "files", fmt.Sprintf("file%05d", i))
		if err := os.WriteFile(path, data, 0600); err != nil {
			b.Fatal(err)
		} else if err := os.Chtimes(path, mtime, mtime); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	var size archiveSizeWriter
	for i := 0; i < b.N; i++ {
		size = 0
		if _, err := ArchiveEntry(context.Background(), root, "files", &size); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(size), "archive-bytes")
}
