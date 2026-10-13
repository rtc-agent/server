package config

import (
	"time"
)

// StorageConfig holds object storage (rtc-oss3) configuration.
// When Backend is empty, OSS3 is completely disabled (S3 endpoint not started, STS API returns 501).
type StorageConfig struct {
	// Backend is the storage backend type: "minio" (reserved: "oss", "cos", "s3").
	// Empty disables OSS3 functionality.
	Backend string `mapstructure:"backend"`

	// MinIO holds MinIO backend configuration.
	MinIO MinIOConfig `mapstructure:"minio"`

	// S3Endpoint holds S3-compatible endpoint configuration.
	S3Endpoint S3EndpointConfig `mapstructure:"s3_endpoint"`

	// Quota holds storage quota configuration.
	Quota QuotaConfig `mapstructure:"quota"`

	// RateLimit holds S3 API rate limiting configuration.
	RateLimit RateLimitConfig `mapstructure:"rate_limit"`

	// Credential holds temporary credential configuration.
	Credential CredentialConfig `mapstructure:"credential"`

	// Cleanup holds periodic cleanup task configuration.
	Cleanup CleanupConfig `mapstructure:"cleanup"`

	// Encryption holds SessionToken encryption configuration.
	Encryption EncryptionConfig `mapstructure:"encryption"`
}

// IsEnabled returns true if a storage backend is configured.
// When backend is empty, OSS3 is completely disabled:
//   - S3 HTTP server is not started (no :9000 port)
//   - STS API endpoints return 501 Not Implemented
//   - Readyz "storage" field returns "not configured" (non-blocking)
//   - Cleanup goroutine is not started
func (c *StorageConfig) IsEnabled() bool {
	return c.Backend != ""
}

// MinIOConfig holds MinIO backend connection configuration.
type MinIOConfig struct {
	// Endpoint is the MinIO server endpoint (e.g., "http://localhost:9000").
	// Used for server-to-MinIO communication (internal network).
	Endpoint string `mapstructure:"endpoint"`

	// PublicURL is the public-facing S3 endpoint URL (e.g., "http://localhost:29000").
	// Used for generating presigned URLs that clients will access (public network).
	// When empty, falls back to Endpoint.
	PublicURL string `mapstructure:"public_url"`

	// AccessKey is the MinIO access key (aligned with docker-compose MINIO_ROOT_USER).
	AccessKey string `mapstructure:"access_key"`

	// SecretKey is the MinIO secret key (aligned with docker-compose MINIO_ROOT_PASSWORD).
	SecretKey string `mapstructure:"secret_key"`

	// Bucket is the bucket name (default "rtc-agent").
	Bucket string `mapstructure:"bucket"`

	// Region is the S3 region for presigned URL generation (e.g., "us-east-1").
	// Used by publicClient to avoid GetBucketLocation calls. Default "us-east-1".
	Region string `mapstructure:"region"`

	// UseSSL enables HTTPS for MinIO connection.
	UseSSL bool `mapstructure:"use_ssl"`

	// HTTP Transport tuning (R7 Review H1).
	// MaxIdleConns is the total idle connections across all hosts. Default 1024.
	MaxIdleConns int `mapstructure:"max_idle_conns"`

	// MaxIdleConnsPerHost is the idle connections per MinIO endpoint. Default 100.
	MaxIdleConnsPerHost int `mapstructure:"max_idle_conns_per_host"`

	// IdleConnTimeout is the timeout for idle connections. Default 90s.
	IdleConnTimeout time.Duration `mapstructure:"idle_conn_timeout"`

	// Retry tuning (R7 Review H5).
	// minio-go defaults: 10 retries with exponential backoff.
	// Override to 3 retries to fail faster and avoid long tail latency.

	// MaxRetries is the max retry attempts per request. Default 3.
	MaxRetries int `mapstructure:"max_retries"`

	// RetryTimeout is the total retry time budget. Default 30s.
	RetryTimeout time.Duration `mapstructure:"retry_timeout"`
}

// S3EndpointConfig holds S3-compatible endpoint configuration.
type S3EndpointConfig struct {
	// Host is the listen address (e.g., "0.0.0.0").
	Host string `mapstructure:"host"`

	// Port is the S3 endpoint port (default 9000).
	Port int `mapstructure:"port"`

	// TLSCert is the TLS certificate path (PEM format). Optional.
	// When both TLSCert and TLSKey are set, S3 endpoint uses HTTPS.
	TLSCert string `mapstructure:"tls_cert"`

	// TLSKey is the TLS private key path (PEM format). Optional.
	TLSKey string `mapstructure:"tls_key"`

	// AllowedOrigins is the CORS whitelist (default ["*"]).
	AllowedOrigins []string `mapstructure:"allowed_origins"`

	// Region is the S3 region for SigV4 signing (default "us-east-1").
	Region string `mapstructure:"region"`
}

// QuotaConfig holds storage quota configuration.
type QuotaConfig struct {
	// MaxFileSizeBytes is the maximum single file size. Default 100MB.
	MaxFileSizeBytes int64 `mapstructure:"max_file_size_bytes"`

	// MaxUserQuotaBytes is the maximum total storage per user. Default 1GB.
	MaxUserQuotaBytes int64 `mapstructure:"max_user_quota_bytes"`

	// MaxConcurrentUploads is the maximum concurrent multipart uploads per user. Default 10.
	MaxConcurrentUploads int `mapstructure:"max_concurrent_uploads"`

	// PendingTTL is the two-phase reserve expiry duration. Default 5m.
	PendingTTL time.Duration `mapstructure:"pending_ttl"`
}

// RateLimitConfig holds S3 API rate limiting configuration.
type RateLimitConfig struct {
	// RequestsPerMinute is the per-user S3 API rate limit. Default 60.
	RequestsPerMinute int `mapstructure:"requests_per_minute"`
}

// CredentialConfig holds temporary credential configuration.
type CredentialConfig struct {
	// AccessTokenTTL is the access token validity duration. Default 1h.
	AccessTokenTTL time.Duration `mapstructure:"access_token_ttl"`

	// SessionTokenTTL is the session token validity duration. Default 1h.
	SessionTokenTTL time.Duration `mapstructure:"session_token_ttl"`

	// PresignedURLTTL is the presigned URL validity duration. Default 1h.
	PresignedURLTTL time.Duration `mapstructure:"presigned_url_ttl"`
}

// CleanupConfig holds periodic cleanup task configuration.
type CleanupConfig struct {
	// Interval is the cleanup task execution interval. Default 1h.
	Interval time.Duration `mapstructure:"interval"`

	// MultipartExpiry is the incomplete multipart upload expiry duration. Default 24h.
	MultipartExpiry time.Duration `mapstructure:"multipart_expiry"`

	// CredentialExpiry is the expired credential cleanup threshold. Default 24h.
	CredentialExpiry time.Duration `mapstructure:"credential_expiry"`

	// Orphan holds orphan object cleanup configuration.
	Orphan OrphanCleanupConfig `mapstructure:"orphan"`
}

// OrphanCleanupConfig holds MinIO orphan object cleanup configuration.
// Orphan objects are MinIO objects with no corresponding DB record, typically
// caused by DB failures after successful MinIO upload.
type OrphanCleanupConfig struct {
	// Enabled controls whether orphan cleanup is active. Default false.
	// Orphan cleanup performs full-bucket scans and should be enabled only
	// after verifying the reconciler does not interfere with normal operations.
	Enabled bool `mapstructure:"enabled"`

	// BatchSize is the number of MinIO objects scanned per batch.
	// Larger batches reduce round-trips but increase memory usage. Default 500.
	BatchSize int `mapstructure:"batch_size"`

	// CooldownPeriod is the minimum age an object must have before it can be
	// deleted. Objects newer than this are skipped to avoid deleting objects
	// that are still being uploaded or committed. Default 1h.
	CooldownPeriod time.Duration `mapstructure:"cooldown_period"`

	// DryRun when true logs orphans without deleting them. Default true.
	// Set to false only after verifying the reconciler identifies correct orphans.
	DryRun bool `mapstructure:"dry_run"`
}

// EncryptionConfig holds SessionToken encryption configuration.
type EncryptionConfig struct {
	// SessionTokenKey is the AES-256 key (32 bytes) for SessionToken encryption.
	// Injected via environment variable OSS3_SESSION_TOKEN_KEY.
	SessionTokenKey string `mapstructure:"session_token_key"`
}
