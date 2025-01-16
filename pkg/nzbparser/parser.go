package nzbparser

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	MetaKeyName     = "Name"
	MetaKeyPassword = "Password"
)

func ParseNzb(inputStream io.Reader) (*NzbData, error) {
	decoder := xml.NewDecoder(inputStream)
	var nzb NzbData

	err := decoder.Decode(&nzb)
	if err != nil {
		return nil, fmt.Errorf("failed decoding nzb: %w", err)
	}

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
		file.SegmentIndexHint = result.SegmentIndexHint
		file.SegmentCountHint = result.SegmentCountHint
	}

	// Handle special meta keys
	if _, ok := nzb.Meta["Name"]; !ok {
		// If Name is not set in meta
		if _, ok := nzb.Meta["Title"]; ok {
			// try title
			nzb.Meta["Name"] = nzb.Meta["Title"]
		} else {
			// lastly try to guess it via file
			// Prefer having a dot in basename
			for i := range nzb.Files {
				file := &nzb.Files[i]

				baseFilename := getBaseFilename(file.Filename)
				if strings.Contains(baseFilename, ".") {
					nzb.Meta["Name"] = baseFilename
				}
			}
			// Take any if there still no match
			if _, ok := nzb.Meta["Name"]; !ok {
				baseFilename := getBaseFilename(nzb.Files[0].Filename)
				nzb.Meta["Name"] = baseFilename
			}
		}
	}
	nzb.MetaName = nzb.Meta["Name"]

	return &nzb, nil
}

var subjectRegexPatterns = []*regexp.Regexp{
	// Detailed
	regexp.MustCompile(`^((?P<Name>.+?) +)?("(?P<Filename>[\w.\-+\[\]() ]+)"|(?P<Filename>[\w.\-+\[\]()]+)) *((?P<Encoding>[\w.\-+\[\]()]+) +)?((?P<TotalSizeHint>[0-9]+) +)?(\((?P<SegmentIndexHint>\d+)\/(?P<SegmentCountHint>\d+)\))?$`),
	// Normal
	regexp.MustCompile(`^((?P<Name>.+?) +)?("(?P<Filename>[\w.\-+\[\]() ]+)") *((?P<Encoding>[\w.\-+\[\]()]+) +)?((?P<TotalSizeHint>[0-9]+) +)?(\((?P<SegmentIndexHint>\d+)\/(?P<SegmentCountHint>\d+)\))?$`),
	// Simple
	regexp.MustCompile(`^.*?"(?P<Filename>[\w.\-+\[\]()]{6,})".*?$`),
	regexp.MustCompile(`^.*?(?P<Filename>[\w.\-+\[\]()]{6,}).*?$`),
	// Primitive
	regexp.MustCompile(`"(?P<Filename>[\w.\-+\[\]() ]+)"`),
}

type ParseResult struct {
	Name             string
	Filename         string
	Encoding         string
	SegmentIndexHint int
	SegmentCountHint int
}

// Static error message
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

			segmentIndexHintStr := getRegexMatchOrDefault(match, regex.SubexpIndex("SegmentIndexHint"), "0")
			segmentCountHintStr := getRegexMatchOrDefault(match, regex.SubexpIndex("SegmentCountHint"), "0")

			var err error
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
