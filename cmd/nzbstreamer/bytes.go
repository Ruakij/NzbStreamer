package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrNotABytes = errors.New("not a byte count")

// Bytes is a byte count written the way one is said: 32M rather than 33554432.
// A K, M, G or T suffix multiplies by a power of 1024, an iB or B after it is
// allowed and means nothing, and a bare number is bytes. envconfig picks it up
// through encoding.TextUnmarshaler.
type Bytes int64

func (b *Bytes) UnmarshalText(text []byte) error {
	value := strings.ToUpper(strings.TrimSpace(string(text)))
	if value == "" {
		*b = 0
		return nil
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "B"), "I")

	multiplier := int64(1)
	if len(value) > 0 {
		if power := strings.IndexByte("KMGT", value[len(value)-1]); power >= 0 {
			multiplier = int64(1) << (10 * (power + 1))
			value = value[:len(value)-1]
		}
	}

	count, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrNotABytes, text)
	}

	*b = Bytes(count * multiplier)
	return nil
}

func (b Bytes) String() string { return strconv.FormatInt(int64(b), 10) }
