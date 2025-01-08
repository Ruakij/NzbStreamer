package nzbParser

import (
	"fmt"
	"regexp"
)

var segmentIdRegex *regexp.Regexp = regexp.MustCompile(``)

type EncapsulatedError struct {
	error
}

func prefixErrors(prefix string, errors []EncapsulatedError) []EncapsulatedError {
	prefixedErrors := make([]EncapsulatedError, len(errors), len(errors))
	for i, err := range errors {
		prefixedErrors[i] = EncapsulatedError{fmt.Errorf("%s%s", prefix, err.error.Error())}
	}
	return prefixedErrors
}

func (nzb *NzbData) CheckPlausability() (warnings []EncapsulatedError, errors []EncapsulatedError) {
	if nzb.MetaName == "" {
		errors = append(errors, EncapsulatedError{
			error: fmt.Errorf("missing Meta.Name"),
		})
	}

	for fileIndex, file := range nzb.Files {
		fileWarnings, fileErrors := file.CheckPlausability()
		warnings = append(warnings, prefixErrors(fmt.Sprintf("Segment[%d]: ", fileIndex), fileWarnings)...)
		errors = append(errors, prefixErrors(fmt.Sprintf("Segment[%d]: ", fileIndex), fileErrors)...)
	}

	return
}

func (file *File) CheckPlausability() (warnings []EncapsulatedError, errors []EncapsulatedError) {
	if file.SegmentCountHint > 0 && len(file.Segments) != file.SegmentCountHint {
		warnings = append(warnings, EncapsulatedError{fmt.Errorf("file.SegmentCountHint differs from len(file.Segments)")})
	}

	if file.Poster == "" {
		warnings = append(warnings, EncapsulatedError{fmt.Errorf("Poster missing")})
	}

	if file.Filename == "" {
		errors = append(errors, EncapsulatedError{fmt.Errorf("Filename missing")})
		return
	}

	hasValidGroup := false
	for i, group := range file.Groups {
		if group == "" {
			warnings = append(warnings, EncapsulatedError{fmt.Errorf("Group[%d] is empty", i)})
		} else {
			hasValidGroup = true
		}
	}
	if !hasValidGroup {
		errors = append(errors, EncapsulatedError{fmt.Errorf("No valid groups")})
		return
	}

	for i, segment := range file.Segments {
		segmentWarnings, segmentErrors := segment.CheckPlausability()
		warnings = append(warnings, prefixErrors(fmt.Sprintf("Segment[%d]: ", i), segmentWarnings)...)
		errors = append(errors, prefixErrors(fmt.Sprintf("Segment[%d]: ", i), segmentErrors)...)
	}

	return
}

func (segment *Segment) CheckPlausability() (warnings []EncapsulatedError, errors []EncapsulatedError) {
	if segment.Id == "" {
		errors = []EncapsulatedError{{fmt.Errorf("Id missing")}}
		return
	}

	if segment.Index == 0 {
		errors = []EncapsulatedError{{fmt.Errorf("Index invalid")}}
		return
	}

	if segment.BytesHint <= 0 {
		errors = []EncapsulatedError{{fmt.Errorf("BytesHint invalid")}}
		return
	}
	return
}
