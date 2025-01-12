package nzbParser

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
		return nil, err
	}

	// Parse meta
	nzb.Meta = make(map[string]string, len(nzb.RawMeta))
	for _, meta := range nzb.RawMeta {
		meta.Type = strings.ToUpper(string(meta.Type[0])) + strings.ToLower(string(meta.Type[1:]))
		nzb.Meta[meta.Type] = meta.Value
	}

	// Parse additional data
	for i, file := range nzb.Files {
		// Parse the date
		nzb.Files[i].ParsedDate = time.Unix(file.Date, 0)

		// Parse the subject
		nzb.Files[i].Displayname, nzb.Files[i].Filename, nzb.Files[i].Encoding, nzb.Files[i].SegmentIndexHint, nzb.Files[i].SegmentCountHint, err = parseSubject(file.Subject)
		if err != nil {
			fmt.Printf("File %d failed parsing step: %v\n", i, err)
		}
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
			for _, file := range nzb.Files {
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

func parseSubject(subject string) (name, filename, encoding string, segmentIndexHint int, segmentCountHint int, err error) {
	for _, regex := range subjectRegexPatterns {
		// Match and check if filename is non-empty
		match := regex.FindStringSubmatch(subject)
		filename = getRegexMatchOrDefault(match, regex.SubexpIndex("Filename"), "")
		if match != nil && strings.Contains(filename, ".") {
			name = getRegexMatchOrDefault(match, regex.SubexpIndex("Name"), "")
			filename = getRegexMatchOrDefault(match, regex.SubexpIndex("Filename"), "")
			encoding = getRegexMatchOrDefault(match, regex.SubexpIndex("Encoding"), "yEnc")

			segmentIndexHintStr := getRegexMatchOrDefault(match, regex.SubexpIndex("SegmentIndexHint"), "0")
			segmentCountHintStr := getRegexMatchOrDefault(match, regex.SubexpIndex("SegmentCountHint"), "0")

			segmentIndexHint, _ = strconv.Atoi(segmentIndexHintStr)
			segmentCountHint, _ = strconv.Atoi(segmentCountHintStr)

			return
		}
	}

	err = fmt.Errorf("Couldnt parse subject '%s'", subject)
	return
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
