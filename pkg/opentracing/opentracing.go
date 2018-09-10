package opentracing

import (
	"context"
	"io"
	"net/http"

	"github.com/hellofresh/gcloud-opentracing"
	"github.com/hellofresh/janus/pkg/config"
	"github.com/opentracing/opentracing-go"
	log "github.com/sirupsen/logrus"
	jaeger "github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	"github.com/uber/jaeger-client-go/zipkin"
	"github.com/uber/jaeger-lib/metrics"
)

const (
	gcloudTracing     = "googleCloud"
	jaegerTracing     = "jaeger"
	zipkinPropagation = "zipkin"
)

// Tracing is the tracing functionality
type Tracing struct {
	config config.Tracing
	tracer opentracing.Tracer
	closer io.Closer
}

type noopCloser struct{}

func (n noopCloser) Close() error { return nil }

// New creates a new instance of Tracing
func New(config config.Tracing) *Tracing {
	return &Tracing{config: config}
}

// Setup a tracer based on the configuration provided
func (t *Tracing) Setup() {
	var err error

	log.Debug("Initializing distributed tracing")
	switch t.config.Provider {
	case gcloudTracing:
		log.Debug("Using google cloud platform (stackdriver trace) as tracing system")
		t.tracer, t.closer, err = t.buildGCloud(t.config.GoogleCloudTracing)
	case jaegerTracing:
		log.Debug("Using Jaeger as tracing system")
		t.tracer, t.closer, err = t.buildJaeger(t.config.ServiceName, t.config.JaegerTracing)
	default:
		log.Debug("No tracer selected")
		t.tracer, t.closer, err = &opentracing.NoopTracer{}, noopCloser{}, nil
	}

	if err != nil {
		log.WithError(err).WithField("provider", t.config.Provider).Warn("Could not initialize tracing")
		return
	}

	opentracing.SetGlobalTracer(t.tracer)
}

// Close tracer
func (t *Tracing) Close() {
	if t.closer != nil {
		t.closer.Close()
	}
}

func (t *Tracing) buildGCloud(config config.GoogleCloudTracing) (opentracing.Tracer, io.Closer, error) {
	tracer, err := gcloudtracer.NewTracer(
		context.Background(),
		gcloudtracer.WithLogger(log.StandardLogger()),
		gcloudtracer.WithProject(config.ProjectID),
		gcloudtracer.WithJWTCredentials(gcloudtracer.JWTCredentials{
			Email:        config.Email,
			PrivateKey:   []byte(config.PrivateKey),
			PrivateKeyID: config.PrivateKeyID,
		}),
	)

	return tracer, noopCloser{}, err
}

func (t *Tracing) buildJaeger(componentName string, c config.JaegerTracing) (opentracing.Tracer, io.Closer, error) {
	cfg := jaegercfg.Configuration{
		Sampler: &jaegercfg.SamplerConfig{
			Type:  c.SamplingType,
			Param: c.SamplingParam,
		},
		Reporter: &jaegercfg.ReporterConfig{
			LogSpans:            c.LogSpans,
			BufferFlushInterval: c.BufferFlushInterval,
			LocalAgentHostPort:  c.SamplingServerURL,
			QueueSize:           c.QueueSize,
		},
	}

	tracerMetrics := jaeger.NewMetrics(metrics.NullFactory, nil)
	tracerLogger := jaegerLoggerAdapter{log.StandardLogger()}
	sampler, err := cfg.Sampler.NewSampler(componentName, tracerMetrics)
	if err != nil {
		return nil, nil, err
	}

	reporter, err := cfg.Reporter.NewReporter(componentName, tracerMetrics, tracerLogger)
	if err != nil {
		return nil, nil, err
	}

	var (
		tracer opentracing.Tracer
		closer io.Closer
	)

	switch c.PropagationFormat {
	case zipkinPropagation:
		log.Debug("Using zipkin b3 http propagation format")
		zipkinPropagator := zipkin.NewZipkinB3HTTPHeaderPropagator()
		tracer, closer = jaeger.NewTracer(componentName, sampler, reporter,
			jaeger.TracerOptions.Metrics(tracerMetrics),
			jaeger.TracerOptions.Logger(tracerLogger),
			jaeger.TracerOptions.Injector(opentracing.HTTPHeaders, zipkinPropagator),
			jaeger.TracerOptions.Extractor(opentracing.HTTPHeaders, zipkinPropagator),
			jaeger.TracerOptions.ZipkinSharedRPCSpan(true),
		)
	default:
		log.Debug("Using jaeger propagation format")
		tracer, closer = jaeger.NewTracer(componentName, sampler, reporter,
			jaeger.TracerOptions.Metrics(tracerMetrics),
			jaeger.TracerOptions.Logger(tracerLogger),
		)
	}

	return tracer, closer, nil
}

// FromContext creates a span from a context that contains a parent span
func FromContext(ctx context.Context, name string) opentracing.Span {
	span, _ := opentracing.StartSpanFromContext(ctx, name)
	return span
}

// ToContext sets a span to a context
func ToContext(r *http.Request, span opentracing.Span) *http.Request {
	return r.WithContext(opentracing.ContextWithSpan(r.Context(), span))
}
