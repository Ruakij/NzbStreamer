package nzbparser

import (
	"errors"
	"fmt"
)

var (
	ErrMissingMetaName          = errors.New("missing Meta.Name")
	ErrDuplicateFilename        = errors.New("duplicate filename")
	ErrSegmentCountHintMismatch = errors.New("file.SegmentCountHint differs from len(file.Segments)")
	ErrPosterMissing            = errors.New("poster missing")
	ErrFilenameMissing          = errors.New("filename missing")
	ErrGroupEmpty               = errors.New("group is empty")
	ErrNoValidGroups            = errors.New("no valid groups")
	ErrIDMissing                = errors.New("id missing")
	ErrIndexInvalid             = errors.New("index invalid")
	ErrBytesHintInvalid         = errors.New("bytesHint invalid")
)

type EncapsulatedError struct {
	error
}

func prefixErrors(prefix string, errors []EncapsulatedError) []EncapsulatedError {
	prefixedErrors := make([]EncapsulatedError, len(errors))
	for i, err := range errors {
		// Static error construction, to reduce dynamic message creation.
		prefixedErrors[i] = EncapsulatedError{
			error: fmt.Errorf("%s%w", prefix, err.error),
		}
	}
	return prefixedErrors
}

func (nzb *NzbData) CheckPlausability() (warnings, errors []EncapsulatedError) {
	if nzb.MetaName == "" {
		errors = append(errors, EncapsulatedError{ErrMissingMetaName})
	}

	filenames := make(map[string]struct{})
	for i := range nzb.Files {
		file := &nzb.Files[i]

		fileWarnings, fileErrors := file.CheckPlausability()
		warnings = append(warnings, prefixErrors(fmt.Sprintf("File '%s': ", file.Filename), fileWarnings)...)
		errors = append(errors, prefixErrors(fmt.Sprintf("File '%s': ", file.Filename), fileErrors)...)

		if _, exists := filenames[file.Filename]; exists {
			errors = append(errors, EncapsulatedError{fmt.Errorf("%s: %w", file.Filename, ErrDuplicateFilename)})
		}
		filenames[file.Filename] = struct{}{}
	}

	return warnings, errors
}

func (file *File) CheckPlausability() (warnings, errors []EncapsulatedError) {
	if file.SegmentCountHint > 0 && len(file.Segments) != file.SegmentCountHint {
		warnings = append(warnings, EncapsulatedError{ErrSegmentCountHintMismatch})
	}

	if file.Poster == "" {
		warnings = append(warnings, EncapsulatedError{ErrPosterMissing})
	}

	if file.Filename == "" {
		errors = append(errors, EncapsulatedError{ErrFilenameMissing})
		return warnings, errors
	}

	hasValidGroup := false
	for i := 0; i < len(file.Groups); i++ {
		group := file.Groups[i]
		if group == "" {
			warnings = append(warnings, EncapsulatedError{ErrGroupEmpty})
		} else {
			hasValidGroup = true
		}
	}
	if !hasValidGroup {
		errors = append(errors, EncapsulatedError{ErrNoValidGroups})
		return warnings, errors
	}

	for i := 0; i < len(file.Segments); i++ {
		segmentWarnings, segmentErrors := file.Segments[i].CheckPlausability()
		warnings = append(warnings, prefixErrors(fmt.Sprintf("Segment %d: ", i), segmentWarnings)...)
		errors = append(errors, prefixErrors(fmt.Sprintf("Segment %d: ", i), segmentErrors)...)
	}

	return warnings, errors
}

func (segment *Segment) CheckPlausability() (warnings, errors []EncapsulatedError) {
	if segment.ID == "" {
		errors = append(errors, EncapsulatedError{ErrIDMissing})
		return warnings, errors
	}

	if segment.Index == 0 {
		errors = append(errors, EncapsulatedError{ErrIndexInvalid})
		return warnings, errors
	}

	if segment.BytesHint <= 0 {
		errors = append(errors, EncapsulatedError{ErrBytesHintInvalid})
		return warnings, errors
	}
	return warnings, errors
}
