package readerAtWrapper

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestReadSeekerBatchedAt_BasicRead(t *testing.T) {
	data := []byte("This is a sample data for testing.")
	buffer := bytes.NewReader(data)
	readSeeker := NewReadSeekerBatchedAt(buffer, 10*time.Millisecond)

	p := make([]byte, 4)
	n, err := readSeeker.ReadAt(p, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 4 || string(p) != "This" {
		t.Fatalf("expected 'This', got %s", string(p))
	}
}

func TestReadSeekerBatchedAt_MultipleReads(t *testing.T) {
	data := []byte("This is a sample data for multiple reads.")
	buffer := bytes.NewReader(data)
	readSeeker := NewReadSeekerBatchedAt(buffer, 10*time.Millisecond)

	requests := []struct {
		offset int64
		eLen   int
		expect string
	}{
		{0, 4, "This"},
		{5, 2, "is"},          // Space is before, should start reading after the space
		{2, 10, "is is a sa"}, // Overlapping read
	}

	for _, req := range requests {
		p := make([]byte, req.eLen)
		n, err := readSeeker.ReadAt(p, req.offset)
		if err != nil || n != req.eLen || string(p) != req.expect {
			t.Errorf("expected %q, got %q, n: %d, err: %v", req.expect, string(p), n, err)
		}
	}
}

func TestReadSeekerBatchedAt_SequentialAndBathedReads(t *testing.T) {
	data := []byte("Sample sequential and batched reading data.")
	buffer := bytes.NewReader(data)
	readSeeker := NewReadSeekerBatchedAt(buffer, 10*time.Millisecond)

	results := make([]result, 3)

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		p1 := make([]byte, 7)
		n, err := readSeeker.ReadAt(p1, 0)
		results[0] = result{n, string(p1), err}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		p3 := make([]byte, 5)
		n, err := readSeeker.ReadAt(p3, 17)
		results[2] = result{n, string(p3), err}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		p2 := make([]byte, 10)
		n, err := readSeeker.ReadAt(p2, 7)
		results[1] = result{n, string(p2), err}
	}()
	wg.Wait()

	if results[0].err != nil || results[0].n != 7 || results[0].data != "Sample " {
		t.Errorf("expected 'Sample '\tgot %q,\tn: %d,\terr: %v", results[0].data, results[0].n, results[0].err)
	}

	if results[1].err != nil || results[1].n != 10 || results[1].data != "sequential" {
		t.Errorf("expected 'sequential'\tgot %q,\tn: %d,\terr: %v", results[1].data, results[1].n, results[1].err)
	}

	if results[2].err != nil || results[2].n != 5 || results[2].data != " and " {
		t.Errorf("expected ' and '\tgot %q,\tn: %d,\terr: %v", results[2].data, results[2].n, results[2].err)
	}
}

type result struct {
	n    int
	data string
	err  error
}

// Tested against incomplete reads and sequential offset.
func TestReadSeekerBatchedAt_EdgeCases(t *testing.T) {
	data := []byte("Edge case testing")
	buffer := bytes.NewReader(data)
	readSeeker := NewReadSeekerBatchedAt(buffer, 10*time.Millisecond)

	p1 := make([]byte, 10)
	_, err := readSeeker.ReadAt(p1, int64(len(data)+10)) // Beyond EOF
	if err == nil {
		t.Errorf("expected error when reading beyond EOF")
	}

	p2 := make([]byte, 5)
	_, err = readSeeker.ReadAt(p2, 0)
	if err != nil || string(p2) != "Edge " {
		t.Errorf("expected 'Edge ', got %s", string(p2))
	}

	p3 := make([]byte, 7)
	_, err = readSeeker.ReadAt(p3, 5)
	if err != nil || string(p3) != "case te" {
		t.Errorf("expected 'case te', got %s", string(p3))
	}
}
