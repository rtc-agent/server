package httphandler

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// handleListObjects handles GET /{bucket} — list objects.
func (h *OSS3Handler) handleListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.list_objects",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("user.id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	// Parse query parameters — supports both V1 (marker) and V2 (list-type=2) semantics.
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	maxKeys := parseMaxKeys(q.Get("max-keys"))

	opts := rtcoss3.ListObjectsOptions{
		Prefix:    prefix,
		Delimiter: delimiter,
		MaxKeys:   maxKeys,
	}

	isV2 := q.Get("list-type") == "2"

	// V1: marker-based pagination
	// V2: continuation-token-based pagination, activated by list-type=2
	if isV2 {
		opts.ContinuationToken = q.Get("continuation-token")
		opts.StartAfter = q.Get("start-after")
	} else {
		opts.Marker = q.Get("marker")
	}

	// List objects from backend
	result, err := h.oss3UC.Backend().ListObjects(r.Context(), bucket, opts)
	if err != nil {
		// Check if this is a continuation token decode error (InvalidArgument)
		if isV2 && opts.ContinuationToken != "" && strings.Contains(err.Error(), "invalid continuation token") {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidArgument, r.URL.Path, "")
			return
		}
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Branch on V1 vs V2 for response serialization
	if isV2 {
		writeListBucketResultV2XML(w, bucket, prefix, q.Get("start-after"), q.Get("continuation-token"), maxKeys, delimiter, result)
	} else {
		writeListBucketResultV1XML(w, bucket, prefix, q.Get("marker"), maxKeys, delimiter, result)
	}
}

// parseMaxKeys parses the "max-keys" query parameter, clamping to [1, maxListKeys].
// Returns the S3 default of defaultMaxKeys when the parameter is absent or invalid.
func parseMaxKeys(raw string) int {
	if raw == "" {
		return defaultMaxKeys
	}
	if mk, err := strconv.Atoi(raw); err == nil && mk > 0 && mk <= maxListKeys {
		return mk
	}
	return defaultMaxKeys
}

// xmlListContent, xmlListCommonPrefix, xmlListBucketResultV1, and xmlListBucketResultV2
// are the XML response types for the ListBucketResult S3 response envelope.
// V1 and V2 have different fields per the S3 specification.
type xmlListContent struct {
	XMLName      xml.Name `xml:"Contents"`
	Key          string   `xml:"Key"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
	Size         int64    `xml:"Size"`
	StorageClass string   `xml:"StorageClass"`
}

type xmlListCommonPrefix struct {
	XMLName xml.Name `xml:"CommonPrefixes"`
	Prefix  string   `xml:"Prefix"`
}

// V1 response: uses Marker/NextMarker for pagination
type xmlListBucketResultV1 struct {
	XMLName        xml.Name              `xml:"ListBucketResult"`
	Name           string                `xml:"Name"`
	Prefix         string                `xml:"Prefix"`
	Marker         string                `xml:"Marker"`
	NextMarker     string                `xml:"NextMarker,omitempty"`
	MaxKeys        int                   `xml:"MaxKeys"`
	Delimiter      string                `xml:"Delimiter,omitempty"`
	IsTruncated    bool                  `xml:"IsTruncated"`
	Contents       []xmlListContent      `xml:"Contents"`
	CommonPrefixes []xmlListCommonPrefix `xml:"CommonPrefixes"`
}

// V2 response: uses ContinuationToken/NextContinuationToken and KeyCount
type xmlListBucketResultV2 struct {
	XMLName               xml.Name              `xml:"ListBucketResult"`
	Name                  string                `xml:"Name"`
	Prefix                string                `xml:"Prefix"`
	StartAfter            string                `xml:"StartAfter,omitempty"`
	ContinuationToken     string                `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string                `xml:"NextContinuationToken,omitempty"`
	KeyCount              int                   `xml:"KeyCount"`
	MaxKeys               int                   `xml:"MaxKeys"`
	Delimiter             string                `xml:"Delimiter,omitempty"`
	IsTruncated           bool                  `xml:"IsTruncated"`
	Contents              []xmlListContent      `xml:"Contents"`
	CommonPrefixes        []xmlListCommonPrefix `xml:"CommonPrefixes"`
}

// writeListBucketResultV1XML serialises a V1 ListBucketResult XML response.
func writeListBucketResultV1XML(
	w http.ResponseWriter,
	bucket, prefix, marker string,
	maxKeys int,
	delimiter string,
	result *rtcoss3.ListObjectsResult,
) {
	resp := xmlListBucketResultV1{
		Name:        bucket,
		Prefix:      prefix,
		Marker:      marker,
		NextMarker:  result.NextMarker,
		MaxKeys:     maxKeys,
		Delimiter:   delimiter,
		IsTruncated: result.IsTruncated,
	}
	for _, obj := range result.Objects {
		resp.Contents = append(resp.Contents, xmlListContent{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(rtcoss3.S3TimeFormat),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, cp := range result.CommonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, xmlListCommonPrefix{
			Prefix: cp,
		})
	}

	writeXMLResponse(w, http.StatusOK, resp)
}

// writeListBucketResultV2XML serialises a V2 ListBucketResult XML response.
func writeListBucketResultV2XML(
	w http.ResponseWriter,
	bucket, prefix, startAfter, continuationToken string,
	maxKeys int,
	delimiter string,
	result *rtcoss3.ListObjectsResult,
) {
	resp := xmlListBucketResultV2{
		Name:                  bucket,
		Prefix:                prefix,
		StartAfter:            startAfter,
		ContinuationToken:     continuationToken,
		NextContinuationToken: result.NextContinuationToken,
		KeyCount:              result.KeyCount,
		MaxKeys:               maxKeys,
		Delimiter:             delimiter,
		IsTruncated:           result.IsTruncated,
	}
	for _, obj := range result.Objects {
		resp.Contents = append(resp.Contents, xmlListContent{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(rtcoss3.S3TimeFormat),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, cp := range result.CommonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, xmlListCommonPrefix{
			Prefix: cp,
		})
	}

	writeXMLResponse(w, http.StatusOK, resp)
}
