package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/disintegration/imaging"
	"github.com/gabriel-vasile/mimetype"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// API limit constants (aligned with reference project apiLimits.ts)
const (
	// APIImageMaxBase64Size is the maximum base64-encoded image size (5MB).
	// Anthropic API hard limit for image content.
	APIImageMaxBase64Size = 5 * 1024 * 1024

	// ImageTargetRawSize is the target raw image size before base64 encoding.
	// 5MB * 3/4 = 3.75MB (base64 adds ~33% overhead)
	ImageTargetRawSize = APIImageMaxBase64Size * 3 / 4

	// ImageMaxWidth is the maximum image width in pixels.
	ImageMaxWidth = 2000

	// ImageMaxHeight is the maximum image height in pixels.
	ImageMaxHeight = 2000

	// ImageMaxReadSize is the maximum raw data size to read from OSS (20MB).
	// Prevents OOM on extremely large images.
	ImageMaxReadSize = 20 * 1024 * 1024

	// MaxTextFileSize is the maximum text file size to load (256KB).
	// Prevents oversized text files from consuming too much context.
	MaxTextFileSize = 256 * 1024

	// TextFileTruncation is appended when a text file exceeds MaxTextFileSize.
	TextFileTruncation = "... [文件已截断，超出 256KB 限制]"
)

// LoadImageFromOSS reads an image from OSS and preprocesses it.
// Returns: processed image bytes, MIME type, error.
//
// The preprocessing pipeline:
// 1. Read raw data from OSS (limited to ImageMaxReadSize)
// 2. Detect MIME type via magic bytes
// 3. Reject WebP/GIF (imaging library doesn't support decoding)
// 4. Decode image with EXIF auto-orientation
// 5. Fast path: if <= 3.75MB and <= 2000x2000, return as-is
// 6. Resize to fit 2000x2000 bounding box
// 7. Multi-level quality degradation: PNG → JPEG [80,60,40,20]
// 8. Fallback: 400x400 JPEG quality=20
// 9. Safety net: verify base64 size <= APIImageMaxBase64Size
func LoadImageFromOSS(ctx context.Context, backend rtcoss3.Backend, bucket, key string) ([]byte, string, error) {
	// 1. Read raw data from OSS
	reader, _, err := backend.GetObject(ctx, bucket, key)
	if err != nil {
		return nil, "", fmt.Errorf("get object from OSS: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Limit read size to prevent OOM
	limited := io.LimitReader(reader, ImageMaxReadSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("read object data: %w", err)
	}
	if int64(len(data)) > ImageMaxReadSize {
		return nil, "", fmt.Errorf("image file exceeds maximum read size (%d MB)", ImageMaxReadSize/(1024*1024))
	}

	// 2. Empty file check
	if len(data) == 0 {
		return nil, "", fmt.Errorf("image file is empty")
	}

	// 3. Magic bytes detection
	mimeType := detectImageFormatFromBuffer(data)
	if mimeType == "" || !strings.HasPrefix(mimeType, "image/") {
		return nil, "", fmt.Errorf("unsupported image format: %s", mimeType)
	}

	// 4. Reject WebP/GIF (imaging library doesn't support decoding)
	if mimeType == "image/webp" {
		return nil, "", fmt.Errorf("WebP format not supported, please convert to JPEG or PNG")
	}
	if mimeType == "image/gif" {
		return nil, "", fmt.Errorf("GIF format not supported, please convert to PNG or JPEG")
	}

	// 5. Decode image with EXIF auto-orientation
	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}

	// 6. Fast path: if small enough, return as-is
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if len(data) <= ImageTargetRawSize && width <= ImageMaxWidth && height <= ImageMaxHeight {
		return data, mimeType, nil
	}

	// 7. Resize to fit 2000x2000 bounding box
	img = imaging.Fit(img, ImageMaxWidth, ImageMaxHeight, imaging.Lanczos)

	// 8. Multi-level quality degradation: PNG → JPEG [80,60,40,20]
	qualities := []int{80, 60, 40, 20}
	var result []byte
	for _, q := range qualities {
		var buf bytes.Buffer
		if err := imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(q)); err != nil {
			continue
		}
		// Check base64-encoded size
		base64Size := base64.StdEncoding.EncodedLen(buf.Len())
		if base64Size <= APIImageMaxBase64Size {
			result = buf.Bytes()
			mimeType = "image/jpeg"
			break
		}
	}

	// 9. Fallback: 400x400 JPEG quality=20
	if result == nil {
		thumb := imaging.Fill(img, 400, 400, imaging.Center, imaging.Lanczos)
		var buf bytes.Buffer
		if err := imaging.Encode(&buf, thumb, imaging.JPEG, imaging.JPEGQuality(20)); err != nil {
			return nil, "", fmt.Errorf("fallback compress failed: %w", err)
		}
		result = buf.Bytes()
		mimeType = "image/jpeg"
	}

	// 10. Safety net: final base64 size check
	base64Size := base64.StdEncoding.EncodedLen(len(result))
	if base64Size > APIImageMaxBase64Size {
		return nil, "", fmt.Errorf("image still exceeds API limit after compression")
	}

	return result, mimeType, nil
}

// detectImageFormatFromBuffer detects image format via magic bytes.
// Uses github.com/gabriel-vasile/mimetype library for robust detection.
//
// Note: This library returns MIME types with parameters (e.g., "text/plain; charset=utf-8")
// and supports more formats than the hand-written magic bytes version in the design doc
// (BMP, TIFF, SVG, etc.). Callers must use strings.HasPrefix(mimeType, "image/") to filter,
// as the library may return non-image types for short or ambiguous inputs.
//
// Edge cases:
//   - Very short inputs (< 12 bytes): returns best-guess MIME (often "text/plain")
//   - image/bmp, image/tiff: detected but imaging library may fail to decode
//   - image/webp, image/gif: detected and explicitly rejected (imaging doesn't support)
func detectImageFormatFromBuffer(data []byte) string {
	mime := mimetype.Detect(data)
	return mime.String()
}

// ImageToInputPart converts raw image bytes to an Eino multimodal content part.
// The eino schema.MessageInputPart uses Image *MessageInputImage (not ImageURL),
// where MessageInputImage embeds MessagePartCommon with Base64Data and MIMEType fields.
func ImageToInputPart(data []byte, mimeType string) schema.MessageInputPart {
	base64Str := base64.StdEncoding.EncodeToString(data)
	return schema.MessageInputPart{
		Type: schema.ChatMessagePartTypeImageURL,
		Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{
				Base64Data: &base64Str,
				MIMEType:   mimeType,
			},
		},
	}
}

// LoadTextFromOSS reads a text file from OSS.
// Returns the UTF-8 text content, truncated if necessary.
func LoadTextFromOSS(ctx context.Context, backend rtcoss3.Backend, bucket, key string) (string, error) {
	reader, _, err := backend.GetObject(ctx, bucket, key)
	if err != nil {
		return "", fmt.Errorf("get object from OSS: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Limit read size to prevent oversized files from consuming context
	limited := io.LimitReader(reader, MaxTextFileSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("read text file: %w", err)
	}

	// Ensure UTF-8 validity
	if !utf8.Valid(data) {
		return "", fmt.Errorf("text file is not valid UTF-8")
	}

	// Convert to string and check if truncation is needed
	content := string(data)
	if len(data) > MaxTextFileSize {
		// UTF-8 safe truncation: roll back to last valid rune boundary
		cutPoint := MaxTextFileSize
		for cutPoint > 0 && !utf8.RuneStart(content[cutPoint]) {
			cutPoint--
		}
		content = content[:cutPoint] + TextFileTruncation
	}
	return content, nil
}

// ExtractFileName extracts the filename from a FileAttachment.
// Priority: Extra["name"] > Extra["filename"] > fallback to last segment of fileid.
// Note: FileAttachment.Extra is *map[string]interface{} (pointer), needs dereference.
func ExtractFileName(file protocol.FileAttachment) string {
	if file.Extra != nil {
		extra := *file.Extra
		if name, ok := extra["name"].(string); ok && name != "" {
			return name
		}
		if name, ok := extra["filename"].(string); ok && name != "" {
			return name
		}
	}
	// Fallback: extract from fileid (format: user-{uuid}/{md5}.{ext})
	parts := strings.Split(file.Fileid, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return file.Fileid
}
