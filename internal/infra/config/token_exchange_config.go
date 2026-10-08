package config

// TokenExchangeConfig Token Exchange configuration following RFC 8693
type TokenExchangeConfig struct {
	// ExternalIssuers list of trusted external JWT issuers that can exchange tokens
	ExternalIssuers []ExternalIssuerConfig `mapstructure:"external_issuers"`
}

// ExternalIssuerConfig describes a trusted external JWT issuer for token exchange
type ExternalIssuerConfig struct {
	// Name human-readable identifier for this issuer
	Name string `mapstructure:"name"`
	// Issuer expected value of the JWT iss claim from this issuer
	Issuer string `mapstructure:"issuer"`
	// JWKSURI URL to fetch the JSON Web Key Set for verifying signatures
	JWKSURI string `mapstructure:"jwks_uri"`
	// AllowedAlgorithms list of permitted JWT signing algorithms
	AllowedAlgorithms []string `mapstructure:"allowed_algorithms"`
	// CacheTTL JWKS cache lifetime in seconds before re-fetching
	CacheTTL int `mapstructure:"cache_ttl"`
	// ClaimsMapping maps standard claim names to custom claim names in the JWT
	ClaimsMapping map[string]string `mapstructure:"claims_mapping"`
}
