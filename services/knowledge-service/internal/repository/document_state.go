package repository

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusReady      = "ready"
	StatusFailed     = "failed"

	MaxDocumentBytes = 50 << 20
	MaxIndexRetries  = 3
)

type UploadError struct {
	Code string
	Msg  string
}

func (e *UploadError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

func unsupportedFormat(name string) error {
	return &UploadError{Code: "E_UNSUPPORTED_FORMAT", Msg: fmt.Sprintf("unsupported file format: %s", filepath.Ext(name))}
}

func emptyDocument() error {
	return &UploadError{Code: "E_EMPTY_DOCUMENT", Msg: "document content is empty"}
}

func documentTooLarge(size, max int64) error {
	return &UploadError{Code: "E_DOCUMENT_TOO_LARGE", Msg: fmt.Sprintf("document size %d exceeds limit %d", size, max)}
}

var allowedExtensions = map[string]bool{
	".pdf": true, ".docx": true, ".md": true, ".markdown": true,
	".txt": true, ".html": true, ".htm": true,
}

func AllowedExtension(filename string) bool {
	return allowedExtensions[strings.ToLower(filepath.Ext(filename))]
}

func ValidateUpload(filename string, content []byte, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = MaxDocumentBytes
	}
	if !AllowedExtension(filename) {
		return unsupportedFormat(filename)
	}
	if len(content) == 0 || strings.TrimSpace(string(content)) == "" {
		return emptyDocument()
	}
	if int64(len(content)) > maxBytes {
		return documentTooLarge(int64(len(content)), maxBytes)
	}
	return nil
}

func ValidStatus(status string) bool {
	switch status {
	case StatusPending, StatusProcessing, StatusReady, StatusFailed:
		return true
	}
	return false
}

func AllowedTransition(from, to string) bool {
	switch from {
	case StatusPending:
		return to == StatusProcessing || to == StatusFailed
	case StatusProcessing:
		return to == StatusReady || to == StatusFailed
	case StatusFailed:
		return to == StatusPending
	case StatusReady:
		return to == StatusProcessing
	}
	return false
}

func Searchable(status string) bool { return status == StatusReady }

func Visible(status string) bool { return ValidStatus(status) }

func Retryable(status string) bool { return status == StatusFailed }

func NextStatusOnFailure(retryCount, maxRetries int) string {
	if maxRetries <= 0 {
		maxRetries = MaxIndexRetries
	}
	if retryCount >= maxRetries {
		return StatusFailed
	}
	return StatusPending
}

func KBMatches(documentKB, requestedKB string) bool {
	if strings.TrimSpace(requestedKB) == "" {
		return true
	}
	if strings.TrimSpace(documentKB) == "" {
		return false
	}
	return documentKB == requestedKB
}
