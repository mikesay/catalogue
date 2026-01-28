package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-kit/log"

	stdopentracing "github.com/opentracing/opentracing-go"
	zipkinot "github.com/openzipkin-contrib/zipkin-go-opentracing"
	"github.com/openzipkin/zipkin-go"
	httpreporter "github.com/openzipkin/zipkin-go/reporter/http"

	"net"
	"net/http"

	"path/filepath"

	"context"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/mikesay/catalogue"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/weaveworks/common/middleware"
)

const (
	ServiceName = "catalogue"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var (
	HTTPLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Time (in seconds) spent serving HTTP requests.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status_code", "isWS"})

	HTTPRequestActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "http_request_active",
		Help: "The number of HTTP requests currently being handled.",
	}, []string{"method", "path"})

	HTTPRequestSizeBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_request_size_bytes",
		Help: "Size of HTTP request bodies in bytes.",
		// Exponential buckets are better for sizes (e.g., 100B to 10MB).
		Buckets: prometheus.ExponentialBuckets(100, 10, 6),
	}, []string{"method", "handler"})

	HTTPResponseSizeBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_response_size_bytes",
		Help:    "Size of HTTP response bodies in bytes.",
		Buckets: prometheus.ExponentialBuckets(100, 10, 6),
	}, []string{"method", "handler"})
)

func init() {
	prometheus.MustRegister(HTTPLatency)
}

func main() {
	var (
		port   = flag.String("port", env("PORT", "80"), "Port to bind HTTP listener") // TODO(pb): should be -addr, default ":80"
		images = flag.String("images", "./images/", "Image path")
		dsn    = flag.String("DSN", "catalogue_user:default_password@tcp(catalogue-db:3306)/socksdb", "Data Source Name: [username[:password]@][protocol[(address)]]/dbname")
		zip    = flag.String("zipkin", os.Getenv("ZIPKIN"), "Zipkin address")
	)
	flag.Parse()

	fmt.Fprintf(os.Stderr, "images: %q\n", *images)
	abs, err := filepath.Abs(*images)
	fmt.Fprintf(os.Stderr, "Abs(images): %q (%v)\n", abs, err)
	pwd, err := os.Getwd()
	fmt.Fprintf(os.Stderr, "Getwd: %q (%v)\n", pwd, err)
	files, _ := filepath.Glob(*images + "/*")
	fmt.Fprintf(os.Stderr, "ls: %q\n", files) // contains a list of all files in the current directory

	// Mechanical stuff.
	errc := make(chan error)
	ctx := context.Background()

	// Log domain.
	var logger log.Logger
	{
		logger = log.NewLogfmtLogger(os.Stderr)
		logger = log.With(logger, "ts", log.DefaultTimestampUTC)
		logger = log.With(logger, "caller", log.DefaultCaller)
	}

	var tracer stdopentracing.Tracer
	{
		// 1. Setup Logger (using modern go-kit/log patterns)
		var logger log.Logger
		{
			logger = log.NewLogfmtLogger(os.Stderr)
			logger = log.With(logger, "ts", log.DefaultTimestampUTC)
			logger = log.With(logger, "caller", log.DefaultCaller)
		}

		// Find service local IP.
		conn, err := net.Dial("udp", "8.8.8.8:80")
		if err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}
		localAddr := conn.LocalAddr().(*net.UDPAddr)
		host := strings.Split(localAddr.String(), ":")[0]
		defer conn.Close()

		if *zip == "" {
			tracer = stdopentracing.NoopTracer{}
		} else {
			// 2. Setup Zipkin Reporter (replaces NewHTTPCollector)
			// Note: Use /api/v2/spans for modern Zipkin
			reporter := httpreporter.NewReporter(*zip)
			defer reporter.Close()

			// 3. Create Local Endpoint
			endpoint, err := zipkin.NewEndpoint(ServiceName, fmt.Sprintf("%v:%v", host, port))
			if err != nil {
				logger.Log("err", err)
				os.Exit(1)
			}

			// 4. Initialize Native Zipkin Tracer
			nativeTracer, err := zipkin.NewTracer(
				reporter,
				zipkin.WithLocalEndpoint(endpoint),
			)
			if err != nil {
				logger.Log("err", err)
				os.Exit(1)
			}

			// 5. Wrap for OpenTracing (replaces zipkin.NewRecorder)
			tracer = zipkinot.Wrap(nativeTracer)
		}

		stdopentracing.SetGlobalTracer(tracer)
	}

	// Data domain.
	db, err := sqlx.Open("mysql", *dsn)
	if err != nil {
		logger.Log("err", err)
		os.Exit(1)
	}
	defer db.Close()

	// Check if DB connection can be made, only for logging purposes, should not fail/exit
	err = db.Ping()
	if err != nil {
		logger.Log("Error", "Unable to connect to Database", "DSN", dsn)
	}

	// Service domain.
	var service catalogue.Service
	{
		service = catalogue.NewCatalogueService(db, logger)
		service = catalogue.LoggingMiddleware(logger)(service)
	}

	// Endpoint domain.
	endpoints := catalogue.MakeEndpoints(service, tracer)

	// HTTP router
	router := catalogue.MakeHTTPHandler(ctx, endpoints, *images, logger, tracer)

	httpMiddleware := []middleware.Interface{
		middleware.Instrument{
			Duration:         HTTPLatency,
			RouteMatcher:     router,
			InflightRequests: HTTPRequestActive,
			RequestBodySize:  HTTPRequestSizeBytes,
			ResponseBodySize: HTTPResponseSizeBytes,
		},
	}

	// Handler
	handler := middleware.Merge(httpMiddleware...).Wrap(router)

	// Create and launch the HTTP server.
	go func() {
		logger.Log("transport", "HTTP", "port", *port)
		errc <- http.ListenAndServe(":"+*port, handler)
	}()

	// Capture interrupts.
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		errc <- fmt.Errorf("%s", <-c)
	}()

	logger.Log("exit", <-errc)
}
