package nzbparser_test

import (
	"bytes"
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
