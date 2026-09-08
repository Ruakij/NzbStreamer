// Package nzbparser parses an nzb document into the files and segments it
// describes, resolving charsets and guessing missing names.
package nzbparser

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/ianaindex"
)

const (
	MetaKeyName     = "Name"
	MetaKeyPassword = "Password"
)

var ErrUnsupportedCharset = errors.New("unsupported charset")

// charsetReader decodes an nzb declaring an encoding other than utf-8; iso-8859-1
// is common enough in the wild that the xml package's utf-8-only default rejects
// real files
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, err := ianaindex.IANA.Encoding(charset)
	if err != nil || enc == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCharset, charset)
	}

	return enc.NewDecoder().Reader(input), nil
}

// ParseNzb reads an nzb. filename is the name of the file it was read from, which
// decides what the nzb is called; pass empty for a source that has none.
func ParseNzb(inputStream io.Reader, filename string) (*NzbData, error) {
	var raw bytes.Buffer
	decoder := xml.NewDecoder(io.TeeReader(inputStream, &raw))
	decoder.CharsetReader = charsetReader
	var nzb NzbData

	err := decoder.Decode(&nzb)
	if err != nil {
		return nil, fmt.Errorf("failed decoding nzb: %w", err)
	}
	// Whatever the decoder pulled through, which is the document plus at most the
	// tail of its last read; re-parsing it ignores the remainder
	nzb.Raw = raw.Bytes()

	// Parse meta
	nzb.Meta = make(map[string]string, len(nzb.RawMeta))
	for _, meta := range nzb.RawMeta {
		meta.Type = strings.ToUpper(string(meta.Type[0])) + strings.ToLower(meta.Type[1:])
		nzb.Meta[meta.Type] = meta.Value
	}

	// Parse additional data
	for i := range nzb.Files {
		file := &nzb.Files[i]

		// Parse the date
		file.ParsedDate = time.Unix(file.Date, 0)

		// Parse the subject
		result, err := parseSubject(file.Subject)
		if err != nil {
			return nil, fmt.Errorf("file %d '%s' failed parsing step: %w", i, file.Subject, err)
		}
		file.Displayname = result.Name
		file.Filename = result.Filename
		file.Encoding = result.Encoding
		file.TotalSizeHint = result.TotalSizeHint
		file.SegmentIndexHint = result.SegmentIndexHint
		file.SegmentCountHint = result.SegmentCountHint
	}

	nzb.MetaName = resolveName(&nzb, filename)

	return &nzb, nil
}

// resolveName decides what an nzb is called.
//
// The file it was read from wins. That name is written outside the post, so it is
// the one part obfuscation cannot reach: an obfuscated nzb names its files, its
// recovery set and often its archive members after the same hash, and every name
// derivable from within it is that hash. It also makes the choice unique for free,
// since a watch folder cannot hold two files of the same name, so two nzbs never
// claim the same tree.
func resolveName(nzb *NzbData, filename string) string {
	switch {
	case filename != "":
		return getBaseFilename(filename)
	case nzb.Meta[MetaKeyName] != "":
		return nzb.Meta[MetaKeyName]
	case nzb.Meta["Title"] != "":
		return nzb.Meta["Title"]
	}

	return guessName(nzb.Files)
}

// What a filename may consist of; `\w` is ascii-only in RE2, so a name carrying an
// umlaut or any other non-ascii letter needs the unicode classes spelled out
const (
	nameChars      = `\p{L}\p{N}_.\-+\[\]()`
	nameCharsSpace = nameChars + ` `
	filename       = `[` + nameCharsSpace + `]+\.[` + nameChars + `]+`
	filenameSpace  = `[` + nameCharsSpace + `]+\.[` + nameCharsSpace + `]+`
)

var subjectRegexPatterns = []*regexp.Regexp{
	// Prefixed; a file counter is the only reliable marker between release name and
	// filename when both carry spaces, a bare dash is not
	regexp.MustCompile(`^((?P<Name>.+?) +)?[\[(]\d+\/\d+[\])] *(- +)?(?P<Filename>` + filename + `) *((?P<Encoding>[` + nameChars + `]+) +)?((?P<TotalSizeHint>[0-9]+) +)?(\((?P<SegmentIndexHint>\d+)\/(?P<SegmentCountHint>\d+)\))?$`),
	// Detailed
	regexp.MustCompile(`^((?P<Name>.+?) +)??(?P<Filename>` + filename + `) *((?P<Encoding>[` + nameChars + `]+) +)?((?P<TotalSizeHint>[0-9]+) +)?(\((?P<SegmentIndexHint>\d+)\/(?P<SegmentCountHint>\d+)\))?$`),
	// Normal
	regexp.MustCompile(`^((?P<Name>.+?) +)?("(?P<Filename>` + filenameSpace + `)") *((?P<Encoding>[` + nameChars + `]+) +)?((?P<TotalSizeHint>[0-9]+) +)?(\((?P<SegmentIndexHint>\d+)\/(?P<SegmentCountHint>\d+)\))?$`),
	// Simple
	regexp.MustCompile(`^.*?"(?P<Filename>[` + nameChars + `]{6,})".*?$`),
	regexp.MustCompile(`^.*?(?P<Filename>[` + nameChars + `]{6,}).*?$`),
	// Primitive
	regexp.MustCompile(`"(?P<Filename>[` + nameCharsSpace + `]+)"`),
}

type ParseResult struct {
	Name             string
	Filename         string
	Encoding         string
	TotalSizeHint    int64
	SegmentIndexHint int
	SegmentCountHint int
}

// ErrCouldNotParseSubject reports a subject no pattern matched a filename in.
var ErrCouldNotParseSubject = fmt.Errorf("could not parse subject")

func parseSubject(subject string) (ParseResult, error) {
	var result ParseResult

	for _, regex := range subjectRegexPatterns {
		match := regex.FindStringSubmatch(subject)
		filename := getRegexMatchOrDefault(match, regex.SubexpIndex("Filename"), "")

		if match != nil && strings.Contains(filename, ".") {
			result.Name = getRegexMatchOrDefault(match, regex.SubexpIndex("Name"), "")
			result.Filename = filename
			result.Encoding = getRegexMatchOrDefault(match, regex.SubexpIndex("Encoding"), "yEnc")

			totalSizeHintStr := getRegexMatchOrDefault(match, regex.SubexpIndex("TotalSizeHint"), "0")
			segmentIndexHintStr := getRegexMatchOrDefault(match, regex.SubexpIndex("SegmentIndexHint"), "0")
			segmentCountHintStr := getRegexMatchOrDefault(match, regex.SubexpIndex("SegmentCountHint"), "0")

			var err error
			result.TotalSizeHint, err = strconv.ParseInt(totalSizeHintStr, 10, 64)
			if err != nil {
				return result, fmt.Errorf("invalid total size hint: %w", err)
			}
			result.SegmentIndexHint, err = strconv.Atoi(segmentIndexHintStr)
			if err != nil {
				return result, fmt.Errorf("invalid segment index hint: %w", err)
			}

			result.SegmentCountHint, err = strconv.Atoi(segmentCountHintStr)
			if err != nil {
				return result, fmt.Errorf("invalid segment count hint: %w", err)
			}

			return result, nil
		}
	}

	return result, ErrCouldNotParseSubject
}

func getRegexMatchOrDefault(match []string, index int, defaultValue string) string {
	if index >= 0 && index < len(match) && match[index] != "" {
		return match[index]
	}
	return defaultValue
}

var filenameExtensionRegexp *regexp.Regexp = regexp.MustCompile(`(\.([\w\-+\[\]()]{1,8}))?$`)

func getBaseFilename(filename string) string {
	return filenameExtensionRegexp.ReplaceAllString(filename, "")
}
