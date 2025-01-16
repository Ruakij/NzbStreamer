package filenameops

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

	for i := range min(len(numsA), len(numsB)) {
		n1, err1 := strconv.Atoi(numsA[i])
		n2, err2 := strconv.Atoi(numsB[i])

		switch {
		case err1 != nil && err2 != nil:
			return 0 // Both errors, treat as equal
		case err1 != nil:
			return -1 // Treat as less if only n1 is an error
		case err2 != nil:
			return 1 // Treat as greater if only n2 is an error
		default:
			// Compare valid integers
			if n1 != n2 {
				return n1 - n2
			}
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
