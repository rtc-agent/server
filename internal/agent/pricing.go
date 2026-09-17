package agent

// ModelPricing defines model pricing in USD per million tokens.
// Different token types have different prices for fine-grained cost calculation.
type ModelPricing struct {
	// InputPerMillion is the price for normal input tokens (USD).
	InputPerMillion float64
	// OutputPerMillion is the price for output tokens (USD).
	OutputPerMillion float64
	// CachedReadPerMillion is the price for cache read (cache hit) tokens (USD), typically 10% of input.
	CachedReadPerMillion float64
	// CachedWritePerMillion is the price for cache write (cache creation) tokens (USD), typically 125% of input.
	CachedWritePerMillion float64
	// ReasoningPerMillion is the price for reasoning (thinking) tokens (USD).
	ReasoningPerMillion float64
}

// FullTokenUsage represents complete token usage data.
// Covers all token type dimensions for cost calculation and Session-level aggregation.
type FullTokenUsage struct {
	// InputTokens is the number of pure input tokens, EXCLUDING cached
	// read/write tokens. Used for cost calculation where cached and uncached
	// prices differ. CachedReadTokens and CachedWriteTokens are tracked
	// separately for fine-grained billing.
	// For total input tokens (including cache), see TokenUsage.InputTokens.
	InputTokens       int64 // pure input (excludes cache read/write)
	OutputTokens      int64
	CachedReadTokens  int64 // cache hit
	CachedWriteTokens int64 // cache creation
	ReasoningTokens   int64 // thinking
	TotalTokens       int64 // all types (aligned with eino TotalTokens)
}

// NewModelPricing creates a ModelPricing from a configuration.
// If cfg is nil, returns the default pricing (Claude 3.5 Sonnet).
func NewModelPricing(cfg *ModelPricingConfig) ModelPricing {
	if cfg == nil {
		return DefaultModelPricing()
	}
	return ModelPricing{
		InputPerMillion:       cfg.InputPerMillion,
		OutputPerMillion:      cfg.OutputPerMillion,
		CachedReadPerMillion:  cfg.CachedReadPerMillion,
		CachedWritePerMillion: cfg.CachedWritePerMillion,
		ReasoningPerMillion:   cfg.ReasoningPerMillion,
	}
}

// ModelPricingConfig is the pricing configuration (aligned with config.ModelPricingConfig to avoid circular imports).
type ModelPricingConfig struct {
	InputPerMillion       float64
	OutputPerMillion      float64
	CachedReadPerMillion  float64
	CachedWritePerMillion float64
	ReasoningPerMillion   float64
}

// DefaultModelPricing returns the default pricing for Claude 3.5 Sonnet.
func DefaultModelPricing() ModelPricing {
	return ModelPricing{
		InputPerMillion:       3.0,
		OutputPerMillion:      15.0,
		CachedReadPerMillion:  0.3,
		CachedWritePerMillion: 3.75,
		ReasoningPerMillion:   0.0,
	}
}

// calculateCostMicros calculates token usage cost in micro-USD (1 USD = 1,000,000 micros).
// Uses float64 for intermediate calculations, converting to int64 micro-USD at the end.
// For current pricing magnitudes (a few dollars per million tokens), float64 precision
// is sufficient for cost tracking needs.
func calculateCostMicros(u *FullTokenUsage, pricing ModelPricing) int64 {
	inputCost := float64(u.InputTokens) / 1_000_000 * pricing.InputPerMillion
	cachedReadCost := float64(u.CachedReadTokens) / 1_000_000 * pricing.CachedReadPerMillion
	cachedWriteCost := float64(u.CachedWriteTokens) / 1_000_000 * pricing.CachedWritePerMillion
	outputCost := float64(u.OutputTokens) / 1_000_000 * pricing.OutputPerMillion
	reasoningCost := float64(u.ReasoningTokens) / 1_000_000 * pricing.ReasoningPerMillion

	totalCostUSD := inputCost + cachedReadCost + cachedWriteCost + outputCost + reasoningCost
	return int64(totalCostUSD * 1_000_000) // convert to micro-USD
}
