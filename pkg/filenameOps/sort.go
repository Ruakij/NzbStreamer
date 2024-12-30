package filenameOps

import (
	"regexp"
	"strconv"
)

var numbersRegex *regexp.Regexp = regexp.MustCompile(`(\d+)`)

func CompareNumberStrings(a, b string) int {
	// Extract numberless strings
	baseA := numbersRegex.ReplaceAllString(a, "")
	baseB := numbersRegex.ReplaceAllString(b, "")

	// Compare the base parts to handle logical sorting
	if baseA != baseB {
		return stringCompare(baseA, baseB)
	}

	// Extract numeric parts using regex
	numsA := numbersRegex.FindAllString(a, -1)
	numsB := numbersRegex.FindAllString(b, -1)

	for i := 0; i < min(len(numsA), len(numsB)); i++ {
		n1, _ := strconv.Atoi(numsA[i])
		n2, _ := strconv.Atoi(numsB[i])

		if n1 != n2 {
			return n1 - n2
		}
	}

	return len(numsA) - len(numsB)
}

func stringCompare(a, b string) int {
	if a < b {
		return -1
	} else if a > b {
		return 1
	}
	return 0
}

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}
