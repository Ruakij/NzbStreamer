// Package yenc decodes the yenc-encoded body of one usenet article.
//
// It reads the dot-stuffed line stream a news server sends, so unstuffing,
// decoding and the crc check are one pass over each line and no line is copied
// between them.
package yenc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
)

var (
	ErrNoHeader  = errors.New("no =ybegin header")
	ErrTruncated = errors.New("body ended before the =yend trailer")
	ErrSize      = errors.New("decoded size does not match the trailer")
	ErrCRC       = errors.New("crc check failed")
)

var (
	ybegin = []byte("=ybegin")
	ypart  = []byte("=ypart")
	yend   = []byte("=yend")
)

// Decode reads one article body up to and including the terminator, so the
// connection it came from is positioned at the next response when this returns.
func Decode(br *bufio.Reader) ([]byte, error) {
	var (
		body     []byte
		started  bool
		escaped  bool
		ended    bool
		capacity int64 = -1
		size     int64 = -1
		want     uint32
	)
	sum := crc32.NewIEEE()

	for {
		line, end, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if end {
			break
		}
		if ended {
			continue // whatever follows the trailer, up to the terminator
		}

		switch {
		case bytes.HasPrefix(line, ybegin):
			// Of a multipart post this is the size of the whole file, which
			// =ypart narrows to the part on the very next line
			capacity = field(line, "size")
			started = true
		case bytes.HasPrefix(line, ypart):
			if begin, end := field(line, "begin"), field(line, "end"); begin > 0 && end >= begin {
				capacity = end - begin + 1
			}
		case bytes.HasPrefix(line, yend):
			size = field(line, "size")
			if crc := field(line, "pcrc32"); crc >= 0 {
				want = uint32(crc)
			}
			ended = true
		case started:
			if body == nil {
				body = alloc(capacity)
			}
			from := len(body)
			body = decodeLine(body, line, &escaped)
			sum.Write(body[from:])
		}
	}

	if !started {
		return nil, ErrNoHeader
	}
	if !ended {
		return nil, ErrTruncated
	}
	if size >= 0 && size != int64(len(body)) {
		return nil, fmt.Errorf("%w: got %d, trailer says %d", ErrSize, len(body), size)
	}
	if want != 0 && sum.Sum32() != want {
		return nil, fmt.Errorf("%w: got %x, trailer says %x", ErrCRC, sum.Sum32(), want)
	}

	return body, nil
}

// maxPrealloc bounds what a header may reserve. A usenet article is well under
// it, and a header claiming more is not worth trusting with an allocation.
const maxPrealloc = 16 << 20

func alloc(size int64) []byte {
	return make([]byte, 0, max(min(size, maxPrealloc), 0))
}

func decodeLine(dst, line []byte, escaped *bool) []byte {
	for _, c := range line {
		switch {
		case *escaped:
			dst = append(dst, c-42-64)
			*escaped = false
		case c == '=':
			*escaped = true
		default:
			dst = append(dst, c-42)
		}
	}

	return dst
}

// readLine returns one unstuffed line without its newline, or end for the
// terminator. The slice points into the readers buffer and is only valid until
// the next call.
func readLine(br *bufio.Reader) (line []byte, end bool, err error) {
	line, err = br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		full := append([]byte(nil), line...)
		for errors.Is(err, bufio.ErrBufferFull) {
			line, err = br.ReadSlice('\n')
			full = append(full, line...)
		}
		line = full
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed reading line: %w", err)
	}

	line = bytes.TrimRight(line, "\r\n")
	if len(line) > 0 && line[0] == '.' {
		return line[1:], len(line) == 1, nil
	}

	return line, false, nil
}

// field is the integer value of key in a yenc header line, hex for a crc and
// -1 where the line does not carry it.
func field(line []byte, key string) int64 {
	for rest := line; ; {
		i := bytes.Index(rest, []byte(key+"="))
		if i < 0 {
			return -1
		}
		// A key is preceded by a space, so "size" does not match "psize"
		if i == 0 || rest[i-1] == ' ' {
			value := rest[i+len(key)+1:]
			if j := bytes.IndexByte(value, ' '); j >= 0 {
				value = value[:j]
			}

			base, bits := 10, 64
			if key == "pcrc32" || key == "crc32" {
				base, bits = 16, 32
			}
			parsed, err := strconv.ParseUint(string(value), base, bits)
			if err != nil {
				return -1
			}

			return int64(parsed)
		}
		rest = rest[i+1:]
	}
}
