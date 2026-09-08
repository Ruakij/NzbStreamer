// Package bytesize is a byte count that reads and prints the way one is said.
package bytesize

import (
	"errors"
	"fmt"
	"math"
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

// String is the count rounded to the largest unit it fills, which is what a log
// line wants: how much, not exactly how many. A count under a kibibyte, and one
// that lands on a whole unit, prints without a fraction.
func (b Bytes) String() string {
	value, unit := float64(b), ""
	for _, next := range []string{"K", "M", "G", "T"} {
		if math.Abs(value) < 1024 {
			break
		}
		value, unit = value/1024, next
	}

	if unit == "" {
		return strconv.FormatInt(int64(b), 10)
	}
	return strconv.FormatFloat(value, 'f', fraction(value), 64) + unit
}

// fraction keeps a digit where it says something: 1.4G is worth more than 1G,
// 512M is not worth 512.0M.
func fraction(value float64) int {
	if value == math.Trunc(value) {
		return 0
	}
	return 1
}
