package nzbfileanalyzer

import (
	"errors"
	"fmt"
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

var ErrSegmentsSizeTooSmall = errors.New("too little resources for test")

// Tries to detect the segment-size used by probing 3 segments; When they have same size, it is assume as the common SegmentSize
func GetProbableUnknownSegmentSize(segmentResources []resource.ReadSeekCloseableResource, testCount int) (int, error) {
	if len(segmentResources) < testCount {
		return 0, ErrSegmentsSizeTooSmall
	}

	commonSegmentSize := 0
	for i := 0; i < 3; i++ {
		segmentResource := segmentResources[i]
		reader, err := segmentResource.Open()
		if err != nil {
			return 0, fmt.Errorf("opening resource failed: %w", err)
		}

		data, err := io.ReadAll(reader)
		if err != nil {
			return 0, fmt.Errorf("reading from resource failed: %w", err)
		}
		segmentSize := len(data)

		if commonSegmentSize == 0 {
			commonSegmentSize = segmentSize
		} else if commonSegmentSize != segmentSize {
			return 0, nil
		}
	}
	return commonSegmentSize, nil
}

var knownSizes = []int{
	716800,
	768000,
	3584000,
}

const (
	segmentYEncOverheadMin float32 = 0.0203435
	segmentYEncOverheadAvg float32 = 0.03114724
	segmentYEncOverheadMax float32 = 0.0453969
)

// Tries to find a known segment-size
func GetProbableKnownSegmentSize(segmentSize int) int {
	for _, knownSize := range knownSizes {
		// Exact
		if segmentSize == knownSize {
			return knownSize
		}
		// Within 5% upwards
		if segmentSize > knownSize && segmentSize <= int(float32(knownSize)*(1+segmentYEncOverheadMax)) {
			return knownSize
		}
	}
	return -1
}

const (
	segmentSizeRatioMin float32 = 1 - segmentYEncOverheadMax
	segmentSizeRatioAvg float32 = 1 - segmentYEncOverheadAvg
	segmentSizeRatioMax float32 = 1 - segmentYEncOverheadMin
)

// Estimate size e.g. when no probable size could be found
func GetEstimatedSegmentSize(segmentSize int) int {
	return int(float32(segmentSize) * segmentSizeRatioMax)
}
