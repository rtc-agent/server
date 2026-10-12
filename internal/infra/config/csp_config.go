package config

// CSPConfig holds Content Security Policy configuration.
// CSP is an HTTP header that helps prevent XSS, clickjacking, and other code injection attacks
// by specifying which sources of content are allowed to be loaded.
type CSPConfig struct {
	// Enabled controls whether CSP headers are active.
	// When false, no CSP headers are set.
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// ReportOnly enables report-only mode (Content-Security-Policy-Report-Only).
	// In this mode, violations are reported but not blocked.
	// Useful for development and testing before enforcing CSP.
	ReportOnly bool `mapstructure:"report_only" yaml:"report_only"`

	// ReportURI is the endpoint where CSP violations are reported.
	// Browsers will POST violation reports to this URI.
	// Example: "/api/csp-report"
	ReportURI string `mapstructure:"report_uri" yaml:"report_uri"`

	// DefaultSrc is the default fallback source for directives not explicitly set.
	// Example: ["'self'"]
	DefaultSrc []string `mapstructure:"default_src" yaml:"default_src"`

	// ScriptSrc controls which JavaScript sources can be executed.
	// Example: ["'self'", "'unsafe-inline'", "'unsafe-eval'"]
	ScriptSrc []string `mapstructure:"script_src" yaml:"script_src"`

	// StyleSrc controls which CSS sources can be applied.
	// Example: ["'self'", "'unsafe-inline'"]
	StyleSrc []string `mapstructure:"style_src" yaml:"style_src"`

	// ImgSrc controls which image sources can be loaded.
	// Example: ["'self'", "data:", "blob:"]
	ImgSrc []string `mapstructure:"img_src" yaml:"img_src"`

	// FontSrc controls which font sources can be loaded.
	// Example: ["'self'", "data:"]
	FontSrc []string `mapstructure:"font_src" yaml:"font_src"`

	// MediaSrc controls which media sources (audio, video) can be loaded.
	// Example: ["'self'", "data:", "blob:"]
	MediaSrc []string `mapstructure:"media_src" yaml:"media_src"`

	// ConnectSrc controls which URLs can be loaded using script interfaces
	// (fetch, XMLHttpRequest, WebSocket, etc.).
	// Example: ["'self'", "ws:", "wss:", "https://api.example.com"]
	ConnectSrc []string `mapstructure:"connect_src" yaml:"connect_src"`

	// FrameSrc controls which URLs can be embedded in frames.
	// Example: ["'self'", "https://trusted.example.com"]
	FrameSrc []string `mapstructure:"frame_src" yaml:"frame_src"`

	// FrameAncestors controls which origins can embed the page in frames.
	// Replaces X-Frame-Options. Use 'none' to prevent all framing.
	// Example: ["'none'"] or ["'self'", "https://parent.example.com"]
	FrameAncestors []string `mapstructure:"frame_ancestors" yaml:"frame_ancestors"`

	// BaseURI controls which URLs can be used in <base> elements.
	// Example: ["'self'"]
	BaseURI []string `mapstructure:"base_uri" yaml:"base_uri"`

	// FormAction controls which URLs can be used as form submission targets.
	// Example: ["'self'"]
	FormAction []string `mapstructure:"form_action" yaml:"form_action"`
}
