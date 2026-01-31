package readers

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ImageReader reads image files and encodes them as base64.
type ImageReader struct{}

// NewImageReader creates a new ImageReader.
func NewImageReader() *ImageReader {
	return &ImageReader{}
}

// ImageResult contains the result of reading an image.
type ImageResult struct {
	Data     []byte
	MimeType string
	Size     int64
}

// Read reads an image file and returns base64-encoded data with MIME type.
func (r *ImageReader) Read(filePath string) (*ImageResult, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat image: %w", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read image: %w", err)
	}

	mimeType := r.getMimeType(filePath)

	return &ImageResult{
		Data:     data,
		MimeType: mimeType,
		Size:     info.Size(),
	}, nil
}

// ReadBase64 reads an image and returns base64-encoded string.
func (r *ImageReader) ReadBase64(filePath string) (string, string, error) {
	result, err := r.Read(filePath)
	if err != nil {
		return "", "", err
	}

	encoded := base64.StdEncoding.EncodeToString(result.Data)
	return encoded, result.MimeType, nil
}

// getMimeType returns the MIME type based on file extension.
func (r *ImageReader) getMimeType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".bmp":
		return "image/bmp"
	case ".ico":
		return "image/x-icon"
	case ".tiff", ".tif":
		return "image/tiff"
	default:
		return "application/octet-stream"
	}
}

// IsSupportedImage checks if the file extension is a supported image type.
func IsSupportedImage(filePath string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico", ".tiff", ".tif":
		return true
	default:
		return false
	}
}
