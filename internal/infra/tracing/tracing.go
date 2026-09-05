package tracing

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Config OpenTelemetry 配置
type Config struct {
	// ServiceName 服务名称
	ServiceName string
	// Endpoint OTLP endpoint (例如: "localhost:4317")
	Endpoint string
	// Insecure 是否使用非加密连接（开发环境）
	Insecure bool
	// SampleRate 采样率 (0.0 - 1.0)
	SampleRate float64
}

// Init 初始化 OpenTelemetry tracer provider
// 返回 shutdown 函数，用于在程序退出时刷新 span 数据
func Init(cfg Config) (func(context.Context) error, error) {
	ctx := context.Background()

	// 创建 OTLP exporter
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	// 创建 resource（标识服务）
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	// 创建 tracer provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithExportTimeout(30*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRate),
		)),
	)

	// 设置为全局 tracer provider
	otel.SetTracerProvider(tp)

	// 设置全局 propagator（支持 W3C Trace Context）
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// 返回 shutdown 函数
	shutdown := func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}

	return shutdown, nil
}
