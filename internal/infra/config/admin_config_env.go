package config

// expandAdminEnvVars expands environment variable references in sensitive admin configuration fields.
// Only applies to fields containing sensitive information (credentials).
func expandAdminEnvVars(cfg *AdminConfig) {
	// SMTP credentials
	cfg.Email.SMTPUser = expandEnvRef(cfg.Email.SMTPUser)
	cfg.Email.SMTPPassword = expandEnvRef(cfg.Email.SMTPPassword)
}
