package nzbParser

import "time"

// File represents a file in the NZB file, including both original and additional parsed data.
type File struct {
	Poster   string    `xml:"poster,attr"`
	Date     int64     `xml:"date,attr"`
	Subject  string    `xml:"subject,attr"`
	Groups   []string  `xml:"groups>group"`
	Segments []Segment `xml:"segments>segment"`
	// Parsed data
	Displayname      string    `xml:"-"`
	Filename         string    `xml:"-"`
	Encoding         string    `xml:"-"`
	PartIndexHint    int       `xml:"-"`
	TotalPartsHint   int       `xml:"-"`
	SegmentIndexHint int       `xml:"-"`
	SegmentCountHint int       `xml:"-"`
	ParsedDate       time.Time `xml:"-"`
}

// Segment represents a segment of a file in the NZB file.
type Segment struct {
	Id        string `xml:",innerxml"`
	Index     int    `xml:"number,attr"`
	BytesHint int    `xml:"bytes,attr"`
}

// NzbData represents the parsed NzbData structure.
type NzbData struct {
	Meta    map[string]string `xml:"-"`
	RawMeta []metadataEntry   `xml:"head>meta"`
	Files   []File            `xml:"file"`
}

// Internal XML Metadata entries
type metadataEntry struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}
