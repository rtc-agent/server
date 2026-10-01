package rtcoss3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Credential represents the parsed credential from Authorization header.
type Credential struct {
	AccessKeyID string
	Date        string // "20260930"
	Region      string
	Service     string
	Scope       string // "20260930/us-east-1/s3/aws4_request"
}

// VerifySigV4Request verifies an S3 request signed with Authorization header.
func VerifySigV4Request(r *http.Request, secretAccessKey string, region string, service string) error {
	// Parse Authorization header
	auth := r.Header.Get("Authorization")
	cred, signedHeaders, providedSig, err := ParseAuthorizationHeader(auth)
	if err != nil {
		return fmt.Errorf("parse authorization header: %w", err)
	}

	// AWS SigV4 requires "host" to be in SignedHeaders
	hostSigned := false
	for _, h := range signedHeaders {
		if h == "host" {
			hostSigned = true
			break
		}
	}
	if !hostSigned {
		return &S3Error{
			Code:     "SignatureDoesNotMatch",
			Message:  "The request must have 'host' in SignedHeaders.",
			HTTPCode: http.StatusForbidden,
		}
	}

	// Validate credential scope.
	// SECURITY: Return ErrInvalidSignature (not a descriptive error) to avoid
	// leaking information about the expected scope format via error messages.
	// This prevents timing side-channel attacks that could infer scope structure
	// from different error paths.
	dateStr := cred.Date
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStr, region, service)
	if cred.Scope != scope {
		return ErrInvalidSignature
	}

	// Check x-amz-content-sha256 header
	contentSHA256 := r.Header.Get("X-Amz-Content-Sha256")
	if contentSHA256 != "UNSIGNED-PAYLOAD" && !isHexDigest(contentSHA256) {
		return fmt.Errorf("unsupported x-amz-content-sha256 value: %s", contentSHA256)
	}

	// R7 Review H3: SigV4 timestamp validation — reject requests outside 15-minute window.
	amzDate := r.Header.Get("X-Amz-Date")
	if err := verifyTimestamp(amzDate, 15*time.Minute); err != nil {
		return err
	}

	// Step 1: CanonicalRequest
	canonicalReq := buildCanonicalRequest(r, signedHeaders, contentSHA256)

	// Step 2: StringToSign
	stringToSign := buildStringToSign(amzDate, scope, canonicalReq)

	// Step 3: Calculate signature
	signingKey := deriveSigningKey(secretAccessKey, dateStr, region, service)
	computedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// Step 4: Compare signatures using constant-time comparison.
	// SECURITY: hmac.Equal is used to prevent timing attacks that could leak
	// information about the expected signature. All validation paths above
	// (parsing, scope check, timestamp check) should complete in similar time
	// to avoid side-channel leaks about signature format or validity.
	if !hmac.Equal([]byte(computedSig), []byte(providedSig)) {
		return ErrInvalidSignature
	}
	return nil
}

// VerifySigV4Presigned verifies a presigned URL request.
func VerifySigV4Presigned(r *http.Request, secretAccessKey string, region string, service string) error {
	q := r.URL.Query()

	// Check expiration
	amzDate := q.Get("X-Amz-Date")
	expires := q.Get("X-Amz-Expires")
	if err := checkPresignedExpiry(amzDate, expires); err != nil {
		return err
	}

	// R7 Review H3: SigV4 timestamp validation
	if err := verifyTimestamp(amzDate, 15*time.Minute); err != nil {
		return err
	}

	// Parse credential
	credStr := q.Get("X-Amz-Credential")
	cred, err := parseCredential(credStr)
	if err != nil {
		return err
	}

	// Validate scope.
	// SECURITY: Return ErrInvalidSignature to avoid leaking scope format information.
	dateStr := cred.Date
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStr, region, service)
	if cred.Scope != scope {
		return ErrInvalidSignature
	}

	// For presigned URLs, payload is always UNSIGNED-PAYLOAD
	signedHeaders := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	sort.Strings(signedHeaders)
	canonicalReq := buildCanonicalRequestForPresign(r, signedHeaders)

	stringToSign := buildStringToSign(amzDate, scope, canonicalReq)
	signingKey := deriveSigningKey(secretAccessKey, dateStr, region, service)
	computedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	providedSig := q.Get("X-Amz-Signature")
	// SECURITY: Use constant-time comparison to prevent timing attacks.
	// See VerifySigV4Request for full security considerations.
	if !hmac.Equal([]byte(computedSig), []byte(providedSig)) {
		return ErrInvalidSignature
	}
	return nil
}

// ParseAuthorizationHeader parses the Authorization header.
// Format: AWS4-HMAC-SHA256 Credential=AKID/20260930/us-east-1/s3/aws4_request,
//
//	SignedHeaders=host;x-amz-content-sha256;x-amz-date,
//	Signature=abc123...
func ParseAuthorizationHeader(auth string) (*Credential, []string, string, error) {
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return nil, nil, "", fmt.Errorf("invalid authorization header prefix")
	}

	parts := strings.Split(auth[17:], ", ")
	if len(parts) != 3 {
		return nil, nil, "", fmt.Errorf("invalid authorization header format: expected 3 parts, got %d", len(parts))
	}

	// Validate each part is non-empty, has the required '=' separator, and
	// contains no leading/trailing commas. This rejects malformed headers
	// with extra commas (e.g. ",, ") which could allow signature bypass.
	for i, part := range parts {
		if part == "" {
			return nil, nil, "", fmt.Errorf("invalid authorization header format: part %d is empty", i)
		}
		if !strings.Contains(part, "=") {
			return nil, nil, "", fmt.Errorf("invalid authorization header format: part %d missing '='", i)
		}
		if strings.HasSuffix(part, ",") || strings.HasPrefix(part, ",") {
			return nil, nil, "", fmt.Errorf("invalid authorization header format: part %d has stray comma", i)
		}
	}

	// Parse Credential
	credPart := strings.TrimPrefix(parts[0], "Credential=")
	if credPart == parts[0] {
		return nil, nil, "", fmt.Errorf("invalid authorization header format: missing Credential= prefix")
	}
	cred, err := parseCredential(credPart)
	if err != nil {
		return nil, nil, "", err
	}

	// Parse SignedHeaders
	signedHeadersPart := strings.TrimPrefix(parts[1], "SignedHeaders=")
	if signedHeadersPart == parts[1] {
		return nil, nil, "", fmt.Errorf("invalid authorization header format: missing SignedHeaders= prefix")
	}
	signedHeaders := strings.Split(signedHeadersPart, ";")
	sort.Strings(signedHeaders)

	// Parse Signature
	signature := strings.TrimPrefix(parts[2], "Signature=")
	if signature == parts[2] {
		return nil, nil, "", fmt.Errorf("invalid authorization header format: missing Signature= prefix")
	}

	return cred, signedHeaders, signature, nil
}

// parseCredential parses a credential string.
// Format: AKID/20260930/us-east-1/s3/aws4_request
func parseCredential(credStr string) (*Credential, error) {
	parts := strings.Split(credStr, "/")
	if len(parts) != 5 {
		return nil, fmt.Errorf("invalid credential format")
	}

	return &Credential{
		AccessKeyID: parts[0],
		Date:        parts[1],
		Region:      parts[2],
		Service:     parts[3],
		Scope:       strings.Join(parts[1:], "/"),
	}, nil
}

// verifyTimestamp checks if the timestamp is within the allowed window.
// The window parameter allows flexibility for testing different time tolerances.
//
//nolint:unparam // window is parameterized for testability
func verifyTimestamp(amzDate string, window time.Duration) error {
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("parse timestamp: %w", err)
	}

	now := time.Now().UTC()
	diff := now.Sub(t)
	if diff < 0 {
		diff = -diff
	}

	if diff > window {
		return &S3Error{
			Code:     "RequestTimeTooSkewed",
			Message:  "The difference between the request time and the server's time is too large.",
			HTTPCode: http.StatusForbidden,
		}
	}
	return nil
}

// checkPresignedExpiry checks if a presigned URL has expired.
func checkPresignedExpiry(amzDate, expires string) error {
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("parse timestamp: %w", err)
	}

	expirySeconds := 0
	if _, err := fmt.Sscanf(expires, "%d", &expirySeconds); err != nil {
		return fmt.Errorf("parse expires: %w", err)
	}

	expiryTime := t.Add(time.Duration(expirySeconds) * time.Second)
	if time.Now().UTC().After(expiryTime) {
		return &S3Error{
			Code:     "AccessDenied",
			Message:  "Request has expired",
			HTTPCode: http.StatusForbidden,
		}
	}
	return nil
}

// buildCanonicalRequest builds the canonical request string.
func buildCanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) string {
	var b strings.Builder

	// HTTPRequestMethod
	b.WriteString(r.Method)
	b.WriteByte('\n')

	// CanonicalURI
	b.WriteString(canonicalURI(r.URL.Path))
	b.WriteByte('\n')

	// CanonicalQueryString
	b.WriteString(canonicalQueryString(r.URL.Query()))
	b.WriteByte('\n')

	// CanonicalHeaders
	b.WriteString(canonicalHeaders(r, signedHeaders))
	b.WriteByte('\n')

	// SignedHeaders
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')

	// HashedPayload
	if payloadHash == "UNSIGNED-PAYLOAD" {
		b.WriteString("UNSIGNED-PAYLOAD")
	} else {
		b.WriteString(payloadHash)
	}

	return b.String()
}

// buildCanonicalRequestForPresign builds canonical request for presigned URLs.
func buildCanonicalRequestForPresign(r *http.Request, signedHeaders []string) string {
	var b strings.Builder

	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(r.URL.Path))
	b.WriteByte('\n')
	b.WriteString(canonicalQueryString(r.URL.Query()))
	b.WriteByte('\n')
	b.WriteString(canonicalHeaders(r, signedHeaders))
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString("UNSIGNED-PAYLOAD")

	return b.String()
}

// canonicalURI returns the canonical URI.
// Per AWS SigV4 spec, each path segment is URI-encoded separately,
// but the "/" separators are preserved.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}

// canonicalQueryString returns the canonical query string.
func canonicalQueryString(query url.Values) string {
	if len(query) == 0 {
		return ""
	}

	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	first := true
	for _, k := range keys {
		values := query[k]
		sort.Strings(values)
		for _, v := range values {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	return b.String()
}

// canonicalHeaders returns the canonical headers.
func canonicalHeaders(r *http.Request, signedHeaders []string) string {
	var b strings.Builder
	for _, h := range signedHeaders {
		b.WriteString(strings.ToLower(h))
		b.WriteByte(':')
		// Special handling for "host" header - use r.Host if Header doesn't have it
		if strings.ToLower(h) == "host" {
			host := r.Host
			if host == "" {
				if r.URL != nil {
					host = r.URL.Host
				}
			}
			b.WriteString(host)
		} else {
			values := r.Header.Values(http.CanonicalHeaderKey(h))
			sort.Strings(values)
			b.WriteString(strings.Join(values, ","))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// buildStringToSign builds the string to sign.
func buildStringToSign(amzDate, scope, canonicalRequest string) string {
	var b strings.Builder
	b.WriteString("AWS4-HMAC-SHA256\n")
	b.WriteString(amzDate)
	b.WriteByte('\n')
	b.WriteString(scope)
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(hashSHA256([]byte(canonicalRequest))))
	return b.String()
}

// deriveSigningKey derives the signing key.
func deriveSigningKey(secretKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// hmacSHA256 computes HMAC-SHA256.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// hashSHA256 computes SHA256 hash.
func hashSHA256(data []byte) []byte {
	h := sha256.New()
	h.Write(data)
	return h.Sum(nil)
}

// isHexDigest checks if a string is a valid hex digest.
func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
