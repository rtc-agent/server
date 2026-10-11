// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/rtc-agent/server/internal/infra/email"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// Sentinel errors for email OTP operations.
var (
	ErrOTPNotFound        = errors.New("verification code not found or expired")
	ErrOTPInvalid         = errors.New("invalid verification code")
	ErrOTPLocked          = errors.New("verification temporarily locked due to too many failed attempts")
	ErrOTPRateLimited     = errors.New("too many send requests, please try again later")
	ErrOTPCooldown        = errors.New("please wait before requesting another code")
	ErrEmailNotRegistered = errors.New("email not registered")
	ErrOTPAlreadyExists   = errors.New("a verification code is already active for this email")
)

// EmailOTPConfig configures OTP behavior.
type EmailOTPConfig struct {
	// TTL is the verification code validity period.
	TTL time.Duration
	// Length is the verification code length.
	Length int
	// SendCooldown is the cooldown period between sending codes to the same email.
	SendCooldown time.Duration
	// MaxSendPerIP is the max OTP send requests per IP per minute.
	MaxSendPerIP int
	// MaxVerifyAttempts is the max failed verification attempts before lockout.
	MaxVerifyAttempts int
	// LockDuration is the lockout duration after exceeding max verify attempts.
	LockDuration time.Duration
}

// Default OTP configuration values to avoid magic numbers.
const (
	defaultOTPTTL            = 10 * time.Minute
	defaultOTPLength         = 6
	defaultSendCooldown      = 60 * time.Second
	defaultMaxSendPerIP      = 5
	defaultMaxVerifyAttempts = 5
	defaultLockDuration      = 15 * time.Minute
)

// EmailOTPUsecase handles email verification code operations.
type EmailOTPUsecase struct {
	adminUserRepo repo.AdminUserRepo
	emailSender   email.Sender
	store         OTPStore
	cfg           EmailOTPConfig
}

// NewEmailOTPUsecase creates a new EmailOTPUsecase.
// Applies default values for any zero-valued config fields.
func NewEmailOTPUsecase(
	adminUserRepo repo.AdminUserRepo,
	emailSender email.Sender,
	store OTPStore,
	cfg EmailOTPConfig,
) *EmailOTPUsecase {
	// Apply defaults for zero-valued config fields
	if cfg.TTL <= 0 {
		cfg.TTL = defaultOTPTTL
	}
	if cfg.Length <= 0 {
		cfg.Length = defaultOTPLength
	}
	if cfg.SendCooldown <= 0 {
		cfg.SendCooldown = defaultSendCooldown
	}
	if cfg.MaxSendPerIP <= 0 {
		cfg.MaxSendPerIP = defaultMaxSendPerIP
	}
	if cfg.MaxVerifyAttempts <= 0 {
		cfg.MaxVerifyAttempts = defaultMaxVerifyAttempts
	}
	if cfg.LockDuration <= 0 {
		cfg.LockDuration = defaultLockDuration
	}

	return &EmailOTPUsecase{
		adminUserRepo: adminUserRepo,
		emailSender:   emailSender,
		store:         store,
		cfg:           cfg,
	}
}

// Close releases resources held by the EmailOTPUsecase.
// If the underlying OTP store implements io.Closer, it will be closed.
func (uc *EmailOTPUsecase) Close() {
	if closer, ok := uc.store.(interface{ Close() }); ok {
		closer.Close()
	}
}

// SendOTP generates and sends a verification code to the given email.
// Returns error if email is not registered, rate limited, or send fails.
func (uc *EmailOTPUsecase) SendOTP(ctx context.Context, emailAddr, clientIP string) error {
	// 1. Check if email is registered (only existing admins can use OTP login)
	_, err := uc.adminUserRepo.GetByEmail(ctx, emailAddr)
	if err != nil {
		if repo.IsNotFound(err) {
			// SECURITY: Return same error as invalid OTP to prevent email enumeration
			logger.Info(ctx, "email_otp.send_email_not_found", zap.String("email", emailAddr))
			return ErrEmailNotRegistered
		}
		return fmt.Errorf("email otp get user: %w", err)
	}

	// 2. Check rate limits
	if err := uc.store.CheckSendAllowed(ctx, emailAddr, clientIP); err != nil {
		logger.Warn(ctx, "email_otp.send_rate_limited",
			zap.String("email", emailAddr),
			zap.String("ip", clientIP),
			zap.Error(err))
		return ErrOTPRateLimited
	}

	// 3. Check if email is locked due to too many failed verification attempts
	if err := uc.store.CheckVerifyAllowed(ctx, emailAddr); err != nil {
		logger.Warn(ctx, "email_otp.send_verify_locked",
			zap.String("email", emailAddr),
			zap.Error(err))
		return ErrOTPLocked
	}

	// 4. Generate OTP
	otp, err := uc.generateOTP()
	if err != nil {
		return fmt.Errorf("email otp generate: %w", err)
	}

	// 5. Store OTP
	if err := uc.store.Store(ctx, emailAddr, otp, uc.cfg.TTL); err != nil {
		if errors.Is(err, ErrOTPAlreadyExists) {
			// An OTP is already active - this shouldn't happen since we checked cooldown,
			// but handle it gracefully
			logger.Warn(ctx, "email_otp.store_race_detected",
				zap.String("email", emailAddr))
			return ErrOTPCooldown
		}
		return fmt.Errorf("email otp store: %w", err)
	}

	// 6. Record send for rate limiting
	uc.store.RecordSend(ctx, emailAddr, clientIP)

	// 7. Send email
	msg := &email.Message{
		To:      emailAddr,
		Subject: "RTC Agent 登录验证码",
		Body:    uc.buildEmailBody(otp),
	}
	if err := uc.emailSender.Send(ctx, msg); err != nil {
		// Email send failed - rollback: delete the stored OTP so user can retry immediately
		uc.store.Delete(ctx, emailAddr)
		logger.Error(ctx, "email_otp.send_failed_rolled_back",
			zap.String("email", emailAddr),
			zap.Error(err))
		return fmt.Errorf("email otp send: %w", err)
	}

	logger.Info(ctx, "email_otp.sent",
		zap.String("email", emailAddr),
		zap.String("ip", clientIP))

	return nil
}

// VerifyOTP verifies the given OTP code and returns nil if valid.
// On successful verification, the OTP is consumed (deleted).
// On failed verification, the attempt is recorded for lockout.
func (uc *EmailOTPUsecase) VerifyOTP(ctx context.Context, emailAddr, otp string) error {
	// 1. Check if email is locked
	if err := uc.store.CheckVerifyAllowed(ctx, emailAddr); err != nil {
		logger.Warn(ctx, "email_otp.verify_locked",
			zap.String("email", emailAddr),
			zap.Error(err))
		return ErrOTPLocked
	}

	// 2. Get stored OTP
	storedOTP, err := uc.store.Get(ctx, emailAddr)
	if err != nil {
		if errors.Is(err, ErrOTPNotFound) {
			return ErrOTPNotFound
		}
		return fmt.Errorf("email otp get: %w", err)
	}

	// 3. Compare OTP using constant-time comparison to prevent timing attacks.
	// Standard string comparison (storedOTP != otp) can leak information about
	// how many characters match through timing differences.
	if subtle.ConstantTimeCompare([]byte(storedOTP), []byte(otp)) != 1 {
		// Record failed attempt
		uc.store.RecordVerifyFailure(ctx, emailAddr)
		logger.Info(ctx, "email_otp.verify_invalid",
			zap.String("email", emailAddr))
		return ErrOTPInvalid
	}

	// 4. OTP is valid - consume it and reset failure counter
	uc.store.Delete(ctx, emailAddr)
	uc.store.ResetVerifyFailures(ctx, emailAddr)

	logger.Info(ctx, "email_otp.verified",
		zap.String("email", emailAddr))

	return nil
}

// generateOTP generates a random numeric OTP of the configured length.
func (uc *EmailOTPUsecase) generateOTP() (string, error) {
	otp := ""
	for i := 0; i < uc.cfg.Length; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		otp += fmt.Sprintf("%d", n.Int64())
	}
	return otp, nil
}

// buildEmailBody builds the HTML email body containing the OTP.
func (uc *EmailOTPUsecase) buildEmailBody(otp string) string {
	return fmt.Sprintf(`
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; line-height: 1.6; color: #333; }
        .container { max-width: 600px; margin: 0 auto; padding: 20px; }
        .otp-code { font-size: 32px; font-weight: bold; letter-spacing: 8px; text-align: center; padding: 20px; background: #f5f5f5; border-radius: 8px; margin: 20px 0; }
        .footer { font-size: 12px; color: #999; margin-top: 30px; }
    </style>
</head>
<body>
    <div class="container">
        <h2>RTC Agent 登录验证码</h2>
        <p>您正在登录 RTC Agent 管理系统，请使用以下验证码：</p>
        <div class="otp-code">%s</div>
        <p>验证码有效期为 %d 分钟。如果您没有请求此验证码，请忽略此邮件。</p>
        <div class="footer">此邮件由系统自动发送，请勿回复。</div>
    </div>
</body>
</html>
`, otp, int(uc.cfg.TTL.Minutes()))
}
