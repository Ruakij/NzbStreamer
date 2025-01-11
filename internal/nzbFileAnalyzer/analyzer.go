package nzbFileAnalyzer

import (
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// Tries to detect the segment-size used by probing 3 segments; When they have same size, it is assume as the common SegmentSize
func GetProbableUnknownSegmentSize(segmentResources []resource.ReadSeekCloseableResource) (commonSegmentSize int, err error) {
	if len(segmentResources) < 3 {
		return
	}

	for i := 0; i < 3; i++ {
		segmentResource := segmentResources[i]
		reader, err := segmentResource.Open()
		if err != nil {
			return 0, err
		}

		data, err := io.ReadAll(reader)
		segmentSize := len(data)

		if commonSegmentSize == 0 {
			commonSegmentSize = segmentSize
		} else {
			if commonSegmentSize != segmentSize {
				return 0, nil
			}
		}
	}
	return commonSegmentSize, nil
}

var (
	knownSizes []int = []int{
		716800,
		768000,
		3584000,
	}
)

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
