package nzbparser_test

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// nzb declaring iso-8859-1, with 0xE4 for the "a-umlaut" in its meta name
var latin1Nzb = []byte("<?xml version=\"1.0\" encoding=\"iso-8859-1\" ?>\n" +
	"<nzb>\n" +
	"<head><meta type=\"name\">M\xe4rchen</meta></head>\n" +
	"<file poster=\"p@example.com\" date=\"1700000000\" subject=\"Release &#34;m\xe4rchen.part01.rar&#34; yEnc (1/2)\">\n" +
	"<groups><group>alt.binaries.test</group></groups>\n" +
	"<segments><segment bytes=\"100\" number=\"1\">a@example.com</segment></segments>\n" +
	"</file>\n</nzb>")

func TestParseNzbDecodesDeclaredCharset(t *testing.T) {
	nzb, err := nzbparser.ParseNzb(bytes.NewReader(latin1Nzb), "")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}

	if nzb.MetaName != "Märchen" {
		t.Fatalf("MetaName = %q, want %q", nzb.MetaName, "Märchen")
	}
}

func TestParseSubjectKeepsNonAsciiFilename(t *testing.T) {
	nzb, err := nzbparser.ParseNzb(bytes.NewReader(latin1Nzb), "")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}

	file := nzb.Files[0]
	if file.Filename != "märchen.part01.rar" {
		t.Fatalf("Filename = %q, want %q", file.Filename, "märchen.part01.rar")
	}
	if file.Displayname != "Release" || file.Encoding != "yEnc" || file.SegmentCountHint != 2 {
		t.Fatalf("subject parsed as name %q, encoding %q, %d segments", file.Displayname, file.Encoding, file.SegmentCountHint)
	}
}

func BenchmarkParse(b *testing.B) {
	doc := benchNzb()
	input := bytes.NewReader(doc)

	b.ReportAllocs()
	b.SetBytes(int64(len(doc)))
	for b.Loop() {
		input.Reset(doc)
		if _, err := nzbparser.ParseNzb(input, ""); err != nil {
			b.Fatalf("ParseNzb: %v", err)
		}
	}
}

func benchNzb() []byte {
	var sb strings.Builder
	sb.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\" ?>\n<nzb>\n<head><meta type=\"name\">release</meta></head>\n")
	for range 5 {
		sb.WriteString("<file poster=\"p@example.com\" date=\"1700000000\" subject=\"Release.part01.rar yEnc (1/1)\">\n<groups><group>alt.binaries.test</group></groups>\n<segments>\n")
		for s := range 10 {
			sb.WriteString("<segment bytes=\"716800\" number=\"")
			sb.WriteString(strconv.Itoa(s + 1))
			sb.WriteString("\">seg@example.com</segment>\n")
		}
		sb.WriteString("</segments>\n</file>\n")
	}
	sb.WriteString("</nzb>")
	return []byte(sb.String())
}
