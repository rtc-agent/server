package rtcoss3

import (
	"context"
	"io"

	"github.com/minio/minio-go/v7"
)

// CreateMultipartUpload initiates a multipart upload.
func (b *MinIOBackend) CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (*MultipartUploadResult, error) {
	uploadID, err := b.core.NewMultipartUpload(ctx, bucket, key, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return nil, mapMinIOError(ctx, err, "create_multipart_upload")
	}
	return &MultipartUploadResult{UploadID: uploadID}, nil
}

// UploadPart uploads a single part.
func (b *MinIOBackend) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (string, error) {
	part, err := b.core.PutObjectPart(ctx, bucket, key, uploadID, partNumber, reader, size, minio.PutObjectPartOptions{})
	if err != nil {
		return "", mapMinIOError(ctx, err, "upload_part")
	}
	return part.ETag, nil
}

// CompleteMultipartUpload finalizes a multipart upload.
func (b *MinIOBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (string, error) {
	coreParts := make([]minio.CompletePart, len(parts))
	for i, p := range parts {
		coreParts[i] = minio.CompletePart{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		}
	}
	info, err := b.core.CompleteMultipartUpload(ctx, bucket, key, uploadID, coreParts, minio.PutObjectOptions{})
	if err != nil {
		return "", mapMinIOError(ctx, err, "complete_multipart_upload")
	}
	return info.ETag, nil
}

// AbortMultipartUpload aborts a multipart upload.
func (b *MinIOBackend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	err := b.core.AbortMultipartUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return mapMinIOError(ctx, err, "abort_multipart_upload")
	}
	return nil
}

// ListParts lists uploaded parts.
// Paginates through all parts using partNumberMarker until IsTruncated is false.
func (b *MinIOBackend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	const maxPartsPerPage = 1000
	var allParts []PartInfo
	partNumberMarker := 0

	for {
		result, err := b.core.ListObjectParts(ctx, bucket, key, uploadID, partNumberMarker, maxPartsPerPage)
		if err != nil {
			return nil, mapMinIOError(ctx, err, "list_parts")
		}

		for _, p := range result.ObjectParts {
			allParts = append(allParts, PartInfo{
				PartNumber:   p.PartNumber,
				Size:         p.Size,
				ETag:         p.ETag,
				LastModified: p.LastModified,
			})
		}

		if !result.IsTruncated {
			break
		}
		// NextPartNumberMarker points to the last part returned; the next
		// call should start *after* it.
		partNumberMarker = result.NextPartNumberMarker
	}

	return allParts, nil
}
