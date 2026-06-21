package models

// AttachmentSummary is the metadata for a file attached to a message, used in API responses.
type AttachmentSummary struct {
	ID       string `json:"id"`
	FileType string `json:"file_type"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}
