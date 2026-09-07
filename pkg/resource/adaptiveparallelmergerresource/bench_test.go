package adaptiveparallelmergerresource_test

import (
	"bytes"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
)

func BenchmarkReadSequential(b *testing.B) {
	var parts []resource.ReadSeekCloseableResource
	var total int64
	for i := range 10 {
		part := &bytesresource.BytesResource{Content: bytes.Repeat([]byte{byte(i)}, 100_000)}
		parts = append(parts, part)
		total += int64(len(part.Content))
	}

	buffer := make([]byte, 64*1024)
	b.ReportAllocs()
	b.SetBytes(total)
	for b.Loop() {
		reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(parts).Open()
		if err != nil {
			b.Fatal(err)
		}
		var read int64
		for {
			n, err := reader.Read(buffer)
			read += int64(n)
			if err != nil {
				break
			}
		}
		reader.Close()
		if read != total {
			b.Fatalf("read %d bytes, want %d", read, total)
		}
	}
}
