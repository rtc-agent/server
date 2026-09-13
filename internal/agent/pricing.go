package agent

// ModelPricing 模型价格配置（USD per million tokens）。
// 不同 token 类型价格不同，用于细粒度成本计算。
type ModelPricing struct {
	// InputPerMillion 正常 input token 价格（USD）
	InputPerMillion float64
	// OutputPerMillion output token 价格（USD）
	OutputPerMillion float64
	// CachedReadPerMillion cache read（cache hit）价格（USD），通常为 input 的 10%
	CachedReadPerMillion float64
	// CachedWritePerMillion cache write（cache creation）价格（USD），通常为 input 的 125%
	CachedWritePerMillion float64
	// ReasoningPerMillion reasoning（thinking）token 价格（USD）
	ReasoningPerMillion float64
}

// FullTokenUsage 完整的 token 使用数据。
// 覆盖所有 token 类型维度，用于成本计算和 Session 级累加。
type FullTokenUsage struct {
	InputTokens       int64 // 纯 input（不含 cache read/write）
	OutputTokens      int64
	CachedReadTokens  int64 // cache hit
	CachedWriteTokens int64 // cache creation
	ReasoningTokens   int64 // thinking
	TotalTokens       int64 // 包含所有类型（与 eino TotalTokens 对齐）
}

// NewModelPricing 从配置创建模型价格配置。
// 如果配置为 nil，返回默认价格（Claude 3.5 Sonnet）。
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

// ModelPricingConfig 模型价格配置（与 config.ModelPricingConfig 对齐，避免循环依赖）
type ModelPricingConfig struct {
	InputPerMillion       float64
	OutputPerMillion      float64
	CachedReadPerMillion  float64
	CachedWritePerMillion float64
	ReasoningPerMillion   float64
}

// DefaultModelPricing 返回 Claude 3.5 Sonnet 的默认价格配置。
func DefaultModelPricing() ModelPricing {
	return ModelPricing{
		InputPerMillion:       3.0,
		OutputPerMillion:      15.0,
		CachedReadPerMillion:  0.3,
		CachedWritePerMillion: 3.75,
		ReasoningPerMillion:   0.0,
	}
}

// calculateCostMicros 计算 token 使用成本，返回微美元（1 USD = 1,000,000 micros）。
// 使用整数避免浮点精度问题。
func calculateCostMicros(u *FullTokenUsage, pricing ModelPricing) int64 {
	inputCost := float64(u.InputTokens) / 1_000_000 * pricing.InputPerMillion
	cachedReadCost := float64(u.CachedReadTokens) / 1_000_000 * pricing.CachedReadPerMillion
	cachedWriteCost := float64(u.CachedWriteTokens) / 1_000_000 * pricing.CachedWritePerMillion
	outputCost := float64(u.OutputTokens) / 1_000_000 * pricing.OutputPerMillion
	reasoningCost := float64(u.ReasoningTokens) / 1_000_000 * pricing.ReasoningPerMillion

	totalCostUSD := inputCost + cachedReadCost + cachedWriteCost + outputCost + reasoningCost
	return int64(totalCostUSD * 1_000_000) // 转换为微美元
}
