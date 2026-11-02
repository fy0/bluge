package zap

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

func BenchmarkChunkedIntCoder(b *testing.B) {
	random := make([]byte, 32*1024)
	_, _ = rand.New(rand.NewSource(1)).Read(random)
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"small", []byte{1, 2, 3, 4}},
		{"compressible", bytes.Repeat([]byte{1, 2, 3, 4}, 128*1024)},
		{"incompressible", random},
	} {
		b.Run(test.name, func(b *testing.B) {
			const chunks = 16
			coder := newChunkedIntCoder(1, chunks-1)
			b.SetBytes(int64(chunks * len(test.data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				coder.Reset()
				for chunk := uint64(0); chunk < chunks; chunk++ {
					if err := coder.AddBytes(chunk, test.data); err != nil {
						b.Fatal(err)
					}
				}
				coder.Close()
				if _, err := coder.Write(io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
