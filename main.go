// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/utils"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	slogotel "github.com/remychantenay/slog-otel"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/codes"
)

const (
	// The slog key for errors.
	errKey            = "error"
	defaultListenPort = "8080"
	// gracefulShutdownSeconds should be higher than NATS client
	// request timeout, and lower than the pod or liveness probe's
	// terminationGracePeriodSeconds.
	gracefulShutdownSeconds = 25
)

// Build-time variables set via ldflags
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

var (
	logger          *slog.Logger
	httpServer      *http.Server
	natsURL         string
	natsConn        *nats.Conn
	jetstreamConn   jetstream.JetStream
	cacheBucketName string
	// TODO: improve the configuration of the service to use dependency injection instead of global variables
	useCache bool
)

func init() {
	natsURL = os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://nats:4222"
	}
	cacheBucketName = os.Getenv("CACHE_BUCKET")
	if cacheBucketName == "" {
		cacheBucketName = "fga-sync-cache"
	}
	useCacheStr := os.Getenv("USE_CACHE")
	if useCacheStr == trueString {
		useCache = true
	}
}

// main parses optional flags and starts the NATS subscribers.
func main() {
	// Allow overriding the port by environmental variable as well as command
	// line argument.
	defaultPort := os.Getenv("PORT")
	if defaultPort == "" {
		defaultPort = defaultListenPort
	}
	var debug = flag.Bool("d", false, "enable debug logging")
	var port = flag.String("p", defaultPort, "health checks port")
	var bind = flag.String("bind", "*", "interface to bind on")

	flag.Usage = func() {
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()

	logOptions := &slog.HandlerOptions{}

	// Optional debug logging.
	if os.Getenv("DEBUG") != "" || *debug {
		logOptions.Level = slog.LevelDebug
		logOptions.AddSource = true
	}

	// Create JSON handler and wrap with slog-otel to add trace_id and span_id from context
	jsonHandler := slog.NewJSONHandler(os.Stdout, logOptions)
	otelHandler := slogotel.OtelHandler{Next: jsonHandler}
	logger = slog.New(otelHandler)
	slog.SetDefault(logger)

	if err := run(*bind, *port); err != nil {
		logger.With(errKey, err).Error("fatal error")
		os.Exit(1)
	}
}

// run contains the main service logic. It is separated from main() so that
// deferred cleanup functions (e.g. OpenTelemetry shutdown) run before
// main() calls os.Exit on error.
func run(bind, port string) error {
	// Set up OpenTelemetry SDK.
	// Command-line/environment OTEL_SERVICE_VERSION takes precedence over
	// the build-time Version variable.
	otelConfig := utils.OTelConfigFromEnv()
	if otelConfig.ServiceVersion == "" {
		otelConfig.ServiceVersion = Version
	}
	otelShutdown, err := utils.SetupOTelSDKWithConfig(context.Background(), otelConfig)
	if err != nil {
		return fmt.Errorf("error setting up OpenTelemetry SDK: %w", err)
	}
	// Handle shutdown properly so nothing leaks.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownSeconds*time.Second)
		defer cancel()
		if shutdownErr := otelShutdown(ctx); shutdownErr != nil {
			logger.With(errKey, shutdownErr).Error("error shutting down OpenTelemetry SDK")
		}
	}()

	// Create an OpenFGA client.
	fgaClient, err := connectFga()
	if err != nil {
		return fmt.Errorf("error creating OpenFGA client: %w", err)
	}

	logger.With("url", os.Getenv("OPENFGA_API_URL")).Info("OpenFGA client created")

	// Create HTTP handlers for health checks.
	createHTTPHandlers()

	startHTTPListener(bind, port)

	// Create a wait group which is used to wait while draining (gracefully
	// closing) a connection.
	gracefulCloseWG := sync.WaitGroup{}

	// Support graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// Create NATS connection.
	gracefulCloseWG.Add(1)
	var natsCloseOnce sync.Once
	natsConn, err = nats.Connect(
		natsURL,
		nats.DrainTimeout(gracefulShutdownSeconds*time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.With(errKey, err).Warn("NATS disconnected with error")
			} else {
				logger.Warn("NATS disconnected")
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.With("url", nc.ConnectedUrl()).Info("NATS reconnected")
		}),
		nats.ErrorHandler(func(_ *nats.Conn, s *nats.Subscription, err error) {
			if s != nil {
				logger.With(errKey, err, "subject", s.Subject, "queue", s.Queue).Error("async NATS error")
			} else {
				logger.With(errKey, err).Error("async NATS error outside subscription")
			}
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			natsCloseOnce.Do(gracefulCloseWG.Done)
			if ctx.Err() != nil {
				logger.Info("NATS closed handler called during graceful shutdown")
				return
			}

			logger.With(
				"lastError", nc.LastError(),
				"stats", nc.Stats(),
			).Error("NATS connection closed unexpectedly")
			select {
			case done <- os.Interrupt:
			default:
			}
		}),
	)
	if err != nil {
		return fmt.Errorf("error creating NATS client: %w", err)
	}
	logger.With("url", natsURL).Info("NATS client created")

	jetstreamConn, err = jetstream.New(natsConn)
	if err != nil {
		return fmt.Errorf("error creating JetStream client: %w", err)
	}
	cacheBucket, err := jetstreamConn.KeyValue(context.Background(), cacheBucketName)
	if err != nil {
		return fmt.Errorf("error binding to cache bucket: %w", err)
	}

	handlerService := HandlerService{
		fgaService: FgaService{
			client:      fgaClient,
			cacheBucket: cacheBucket,
		},
	}

	if err = createQueueSubscriptions(handlerService); err != nil {
		return fmt.Errorf("error creating queue subscriptions: %w", err)
	}

	if err = startMaxDeliveryAdvisorySubscription(ctx, natsConn, jetstreamConn); err != nil {
		return fmt.Errorf("error starting max-delivery advisory subscription: %w", err)
	}

	accessMutationConsumer, err := startAccessMutationConsumer(ctx, jetstreamConn, handlerService)
	if err != nil {
		return fmt.Errorf("error starting access mutation consumer: %w", err)
	}

	// This next line blocks until SIGINT or SIGTERM is received, or NATS closes.
	<-done

	stopAccessMutationConsumer(accessMutationConsumer, cancel)

	// Drain the connection, which will drain all subscriptions, then close the
	// connection when complete.
	if !natsConn.IsClosed() && !natsConn.IsDraining() {
		logger.Info("draining NATS connections")
		if err = natsConn.Drain(); err != nil {
			return fmt.Errorf("error draining NATS connection: %w", err)
		}
	}

	// Wait for the graceful shutdown steps to complete.
	gracefulCloseWG.Wait()

	// Immediately close the HTTP server after graceful shutdown has finished.
	if err = httpServer.Close(); err != nil {
		logger.With(errKey, err).Error("http listener error on close")
	}

	return nil
}

func startHTTPListener(bind, port string) {
	// Add an http listener for health checks. This server does NOT participate
	// in the graceful shutdown process; we want it to stay up until the process
	// is killed, to avoid liveness checks failing during the graceful shutdown.
	var addr string
	if bind == "*" {
		addr = ":" + port
	} else {
		addr = bind + ":" + port
	}
	// Wrap the handler with OpenTelemetry instrumentation
	handler := otelhttp.NewHandler(http.DefaultServeMux, "fga-sync",
		otelhttp.WithFilter(func(r *http.Request) bool {
			p := r.URL.Path
			return p != "/livez" && p != "/readyz"
		}),
	)

	httpServer = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 3 * time.Second,
	}
	go func() {
		logger.Info("starting HTTP server", "addr", addr)
		err := httpServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			logger.With(errKey, err).Error("http listener error")
		}
	}()
}

// createHTTPHandlers creates HTTP handlers for health checks.
func createHTTPHandlers() {
	// Support GET/POST monitoring "ping".
	http.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		// This always returns OK as long as the process is running. NATS
		// reconnects indefinitely rather than exiting on connection loss, so
		// connectivity health is reported via /readyz instead.
		_, err := fmt.Fprintf(w, "OK\n")
		if err != nil {
			logger.With(errKey, err).Error("error writing to response writer")
		}
	})

	// Basic health check.
	http.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if natsConn == nil {
			http.Error(w, "no NATS connection", http.StatusServiceUnavailable)
			return
		}
		if !natsConn.IsConnected() || natsConn.IsDraining() {
			http.Error(w, "NATS connection not ready", http.StatusServiceUnavailable)
			return
		}
		_, err := fmt.Fprintf(w, "OK\n")
		if err != nil {
			logger.With(errKey, err).Error("error writing to response writer")
		}
	})
}

// HandlerFunc defines a message handler function type.
type HandlerFunc func(context.Context, INatsMsg) error

// subscriptionConfig defines a NATS subscription configuration.
type subscriptionConfig struct {
	subject     string
	handler     HandlerFunc
	description string
}

// subscribeToSubject subscribes to a single NATS subject with error handling and logging.
func subscribeToSubject(subject, description, queue string, handler HandlerFunc) error {
	if _, err := natsConn.QueueSubscribe(subject, queue, func(msg *nats.Msg) {
		// A fresh background context is used as the extraction base, not the
		// service context, since this callback must not inherit shutdown
		// cancellation ahead of any explicit handling of that signal.
		msgCtx, span := startConsumerSpan(context.Background(), msg.Header, subject)
		defer span.End()
		if errHandler := handler(msgCtx, &NatsMsg{msg}); errHandler != nil {
			span.RecordError(errHandler)
			span.SetStatus(codes.Error, errHandler.Error())
			logger.Error("error handling "+description+" request",
				errKey, errHandler,
				"subject", subject,
				"queue", queue,
			)
		}
	}); err != nil {
		logger.Error("error subscribing to NATS subject",
			errKey, err,
			"subject", subject,
			"queue", queue,
		)
		return err
	}
	logger.Info("subscribed to NATS subject",
		"subject", subject,
		"queue", queue,
	)
	return nil
}

// createQueueSubscriptions creates queue subscriptions for the NATS subjects.
func createQueueSubscriptions(handlerService HandlerService) error {
	queue := constants.FgaSyncQueue

	for _, config := range queueSubscriptionConfigs(handlerService) {
		if err := subscribeToSubject(config.subject, config.description, queue, config.handler); err != nil {
			return err
		}
	}

	return nil
}

// queueSubscriptionConfigs lists core NATS (non-JetStream) subscriptions only.
// member_put and member_remove moved to the shared JetStream access-mutation
// consumer in release 2/3 of the fga-sync-jetstream-membership change; do not
// add them back here. See
// openspec/changes/fga-sync-jetstream-membership/tasks.md group 7 — this
// change must not deploy before group 6 (stream widening) is applied and
// verified live, or membership messages are lost permanently.
func queueSubscriptionConfigs(handlerService HandlerService) []subscriptionConfig {
	return []subscriptionConfig{
		{
			subject:     constants.AccessCheckSubject,
			handler:     handlerService.accessCheckHandler,
			description: "access check",
		},
		{
			subject:     constants.ReadTuplesSubject,
			handler:     handlerService.readTuplesHandler,
			description: "read tuples",
		},
	}
}
