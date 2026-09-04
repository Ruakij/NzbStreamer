package yenc_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand"
	"strings"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/yenc"
)

// encode is the article a news server would send: yenc-encoded, dot-stuffed and
// terminated.
func encode(data []byte, cols int) string {
	var body bytes.Buffer
	line := make([]byte, 0, cols*2)

	flush := func() {
		if len(line) > 0 && line[0] == '.' {
			body.WriteByte('.')
		}
		body.Write(line)
		body.WriteString("\r\n")
		line = line[:0]
	}

	for _, b := range data {
		c := b + 42
		if c == 0 || c == '\n' || c == '\r' || c == '=' {
			line = append(line, '=', c+64)
		} else {
			line = append(line, c)
		}
		if len(line) >= cols {
			flush()
		}
	}
	if len(line) > 0 {
		flush()
	}

	return fmt.Sprintf("=ybegin part=1 line=%d size=%d name=test.bin\r\n", cols, len(data)) +
		fmt.Sprintf("=ypart begin=1 end=%d\r\n", len(data)) +
		body.String() +
		fmt.Sprintf("=yend size=%d part=1 pcrc32=%08x\r\n.\r\n", len(data), crc32.ChecksumIEEE(data)) +
		"201 next response\r\n"
}

func TestDecodeRoundTrip(t *testing.T) {
	data := make([]byte, 40_000)
	if _, err := rand.New(rand.NewSource(1)).Read(data); err != nil {
		t.Fatal(err)
	}
	// A line starting with the escape of a NUL and one starting with a dot,
	// which are the two cases a plain copy gets wrong
	data[0], data[128] = 214, 4

	reader := bufio.NewReader(strings.NewReader(encode(data, 128)))

	got, err := yenc.Decode(reader)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(data))
	}

	// Decode consumes the terminator, so the connection is on the next response
	if next, err := reader.ReadString('\n'); err != nil || next != "201 next response\r\n" {
		t.Errorf("left the stream at %q (%v)", next, err)
	}
}

// The size on =ybegin is the whole file, so one part of a large one must not
// reserve the whole file to hold its own few hundred kilobytes.
func TestDecodeAllocatesForThePartNotTheFile(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 1000)
	article := strings.Replace(encode(data, 128),
		fmt.Sprintf("size=%d name=", len(data)), "size=800000000 name=", 1)

	got, err := yenc.Decode(bufio.NewReader(strings.NewReader(article)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("decoded content does not match")
	}
	if cap(got) > 4*len(data) {
		t.Errorf("reserved %d bytes for a part of %d", cap(got), len(data))
	}
}

func TestDecodeRejectsATruncatedBody(t *testing.T) {
	article := encode(bytes.Repeat([]byte{7}, 1000), 128)
	cut := article[:strings.Index(article, "=yend")]

	if _, err := yenc.Decode(bufio.NewReader(strings.NewReader(cut))); err == nil {
		t.Error("a body cut short decoded without an error")
	}
}

func TestDecodeRejectsABadCRC(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 1000)
	article := strings.Replace(encode(data, 128),
		fmt.Sprintf("pcrc32=%08x", crc32.ChecksumIEEE(data)), "pcrc32=deadbeef", 1)

	if _, err := yenc.Decode(bufio.NewReader(strings.NewReader(article))); !errors.Is(err, yenc.ErrCRC) {
		t.Errorf("error is %v, want a crc failure", err)
	}
}
