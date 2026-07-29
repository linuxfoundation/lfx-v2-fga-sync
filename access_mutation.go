// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// accessMutationProcessingTimeout bounds a single delivery attempt. It is kept
// shorter than the first BackOff step so a stalled attempt cannot overlap its
// own redelivery.
const accessMutationProcessingTimeout = 90 * time.Second

// accessMutationAdvisoryLookupTimeout bounds the Stream.GetMsg call used to
// enrich a max-delivery advisory with its object context, so a JetStream
// stall cannot block the advisory callback indefinitely. sync_max_deliver_exhausted
// is incremented before this lookup, so a timeout here still records the
// exhaustion; only the enrichment is best-effort.
const accessMutationAdvisoryLookupTimeout = 5 * time.Second

// accessMutationShutdownGrace bounds how long stopAccessMutationConsumer waits
// for an in-flight delivery attempt to finish on its own before force-
// canceling its context. OpenFGA writes normally complete in well under a
// second, so this lets a near-complete attempt succeed on shutdown instead of
// being aborted and paying a full BackOff cycle on redelivery, while still
// bounding total shutdown time if an attempt is genuinely stuck. It is a var,
// not a const, so tests can shorten it instead of waiting out the real value.
var accessMutationShutdownGrace = 5 * time.Second

// accessMutationRecoveryInterval avoids a hot loop while keeping recovery from
// a deleted durable consumer quick. It is a var so tests can shorten it.
var accessMutationRecoveryInterval = 2 * time.Second

var (
	syncAck                 = expvar.NewInt("sync_ack")
	syncTransientAttempts   = expvar.NewInt("sync_transient_attempts")
	syncTerminal            = expvar.NewInt("sync_terminal")
	syncMaxDeliverExhausted = expvar.NewInt("sync_max_deliver_exhausted")
)

// terminalValidationError marks an error as a proven, locally detected
// validation failure that must terminate JetStream redelivery (TERM) rather
// than being retried as transient.
type terminalValidationError struct {
	err error
}

func (e *terminalValidationError) Error() string {
	return e.err.Error()
}

func (e *terminalValidationError) Unwrap() error {
	return e.err
}

// newTerminalValidationError wraps err so isTerminalValidationError reports true.
func newTerminalValidationError(err error) error {
	return &terminalValidationError{err: err}
}

// isTerminalValidationError reports whether err (or a wrapped cause) was
// produced by newTerminalValidationError.
func isTerminalValidationError(err error) bool {
	var terminalErr *terminalValidationError
	return errors.As(err, &terminalErr)
}

// safeErrorType returns a sanitized, low-cardinality string describing err's
// classification, suitable for logs and trace attributes without leaking
// payload contents.
func safeErrorType(err error) string {
	var statusErr fgaStatusCoder
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("openfga_http_%d", statusErr.ResponseStatusCode())
	}
	if isTerminalValidationError(err) {
		return "terminal_validation"
	}
	return fmt.Sprintf("%T", err)
}

// accessMutationMessage is the subset of [jetstream.Msg] the consumer needs to
// process and acknowledge an access mutation delivery.
type accessMutationMessage interface {
	jetStreamMessage
	Metadata() (*jetstream.MsgMetadata, error)
	Ack() error
	Term() error
}

var _ accessMutationMessage = jetstream.Msg(nil)

// accessMutationConsumeContext is the subset of [jetstream.ConsumeContext]
// needed for ordered shutdown.
type accessMutationConsumeContext interface {
	Stop()
	Closed() <-chan struct{}
}

type accessMutationConsumerFactory interface {
	CreateOrUpdateConsumer(
		context.Context,
		string,
		jetstream.ConsumerConfig,
	) (jetstream.Consumer, error)
}

// accessMutationConsumerManager owns the current consume context and recreates
// it if the shared durable consumer is deleted while the service is running.
type accessMutationConsumerManager struct {
	ctx            context.Context
	factory        accessMutationConsumerFactory
	handlerService HandlerService
	current        jetstream.ConsumeContext
	recover        chan struct{}
	stop           chan struct{}
	closed         chan struct{}
	stopOnce       sync.Once
	generation     atomic.Uint64
	deleted        atomic.Uint64
}

// accessMutationConsumerConfig returns the durable pull consumer configuration
// shared by update_access and delete_access. MaxAckPending of 1 serializes
// delivery so full-state updates for an object are never applied out of order;
// BackOff spaces redelivery attempts over roughly 94 minutes before the
// max-delivery advisory fires.
//
// DeliverNewPolicy is equivalent to DeliverAllPolicy for the empty stream used
// at the initial cutover. On normal restarts the existing durable resumes from
// its stored cursor. If the durable state is ever lost, recreating it starts at
// the current stream tail instead of replaying retained history whose prior
// disposition is unknown. This favors availability and avoids stale updates
// recreating authorization after a later deletion.
func accessMutationConsumerConfig() jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Name:          constants.FgaSyncAccessMutationConsumerName,
		Durable:       constants.FgaSyncAccessMutationConsumerName,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    7,
		BackOff: []time.Duration{
			2 * time.Minute,
			2 * time.Minute,
			5 * time.Minute,
			10 * time.Minute,
			15 * time.Minute,
			30 * time.Minute,
		},
		MaxAckPending: 1,
	}
}

// startAccessMutationConsumer creates or updates the durable consumer and
// begins consuming access mutation messages.
func startAccessMutationConsumer(
	ctx context.Context,
	factory accessMutationConsumerFactory,
	handlerService HandlerService,
) (accessMutationConsumeContext, error) {
	manager := &accessMutationConsumerManager{
		ctx:            ctx,
		factory:        factory,
		handlerService: handlerService,
		recover:        make(chan struct{}, 1),
		stop:           make(chan struct{}),
		closed:         make(chan struct{}),
	}
	if err := manager.startCurrent(); err != nil {
		return nil, err
	}
	go manager.run()
	return manager, nil
}

func (m *accessMutationConsumerManager) startCurrent() error {
	generation := m.generation.Add(1)
	consumer, err := m.factory.CreateOrUpdateConsumer(
		m.ctx,
		constants.FgaSyncStreamName,
		accessMutationConsumerConfig(),
	)
	if err != nil {
		return err
	}

	m.current, err = consumer.Consume(
		func(message jetstream.Msg) {
			processAccessMutationMessage(m.ctx, &m.handlerService, message)
		},
		jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, consumeErr error) {
			m.handleConsumeErrorForGeneration(m.ctx, consumeErr, generation)
		}),
	)
	return err
}

func (m *accessMutationConsumerManager) handleConsumeError(ctx context.Context, err error) {
	m.handleConsumeErrorForGeneration(ctx, err, m.generation.Load())
}

func (m *accessMutationConsumerManager) handleConsumeErrorForGeneration(
	ctx context.Context,
	err error,
	generation uint64,
) {
	logger.With(errKey, err).ErrorContext(ctx, "JetStream consumer error")
	if errors.Is(err, jetstream.ErrConsumerDeleted) {
		m.requestRecovery(generation)
	}
}

func (m *accessMutationConsumerManager) requestRecovery(generation uint64) {
	for deleted := m.deleted.Load(); deleted < generation; deleted = m.deleted.Load() {
		if m.deleted.CompareAndSwap(deleted, generation) {
			break
		}
	}
	select {
	case m.recover <- struct{}{}:
	default:
	}
}

func (m *accessMutationConsumerManager) run() {
	defer close(m.closed)
	for {
		select {
		case <-m.stop:
			m.stopCurrent()
			return
		case <-m.recover:
			if m.deleted.Load() < m.generation.Load() {
				continue
			}
			if !m.recoverCurrent() {
				return
			}
		}
	}
}

func (m *accessMutationConsumerManager) recoverCurrent() bool {
	select {
	case <-m.current.Closed():
	case <-m.stop:
		m.stopCurrent()
		return false
	}

	for {
		select {
		case <-m.stop:
			return false
		default:
		}

		if err := m.startCurrent(); err == nil {
			select {
			case <-m.stop:
				m.stopCurrent()
				return false
			default:
			}
			return true
		} else {
			logger.With(errKey, err).ErrorContext(m.ctx, "failed to recreate JetStream consumer")
		}

		timer := time.NewTimer(accessMutationRecoveryInterval)
		select {
		case <-timer.C:
		case <-m.stop:
			timer.Stop()
			return false
		}
	}
}

func (m *accessMutationConsumerManager) stopCurrent() {
	if m.current == nil {
		return
	}
	m.current.Stop()
	<-m.current.Closed()
}

func (m *accessMutationConsumerManager) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
}

func (m *accessMutationConsumerManager) Closed() <-chan struct{} {
	return m.closed
}

// accessMutationAttemptContext bounds a single delivery attempt.
func accessMutationAttemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, accessMutationProcessingTimeout)
}

// stopAccessMutationConsumer stops the consume loop and waits for it to fully
// close. ConsumeContext.Closed() only fires once any in-flight delivery
// attempt has returned, so canceling the shared context before that would
// abort an attempt that might otherwise succeed. Cancellation is therefore
// deferred to a bounded grace period: give the in-flight attempt a chance to
// finish normally, and only force-cancel if it does not.
func stopAccessMutationConsumer(
	consumer accessMutationConsumeContext,
	cancel context.CancelFunc,
) {
	consumer.Stop()
	select {
	case <-consumer.Closed():
		// The in-flight attempt, if any, finished on its own; nothing is
		// using the context anymore, so canceling now is safe.
		cancel()
	case <-time.After(accessMutationShutdownGrace):
		// The attempt did not finish in time; force it to abort so the
		// consume loop can close.
		cancel()
		<-consumer.Closed()
	}
}

// processAccessMutationMessage dispatches a single delivery attempt and
// acknowledges, terminates, or leaves it unacknowledged based on the outcome.
func processAccessMutationMessage(
	ctx context.Context,
	handlerService *HandlerService,
	message accessMutationMessage,
) {
	msgCtx, span := startConsumerSpan(ctx, message.Headers(), message.Subject())
	defer span.End()

	msgCtx, cancel := accessMutationAttemptContext(msgCtx)
	defer cancel()

	err := dispatchAccessMutation(msgCtx, handlerService, newJetStreamNatsMsg(message))
	if err != nil {
		recordAccessMutationError(msgCtx, err)
	}
	switch {
	case err == nil:
		ackAccessMutation(msgCtx, message)
	case isTerminalValidationError(err):
		terminateAccessMutation(msgCtx, message, err)
	default:
		syncTransientAttempts.Add(1)
		logAccessMutationFailure(msgCtx, message, "transient", err)
	}
}

// dispatchAccessMutation routes a message to its handler by subject.
func dispatchAccessMutation(
	ctx context.Context,
	handlerService *HandlerService,
	message INatsMsg,
) error {
	switch message.Subject() {
	case constants.GenericUpdateAccessSubject:
		return handlerService.genericUpdateAccessHandler(ctx, message)
	case constants.GenericDeleteAccessSubject:
		return handlerService.genericDeleteAccessHandler(ctx, message)
	default:
		return newTerminalValidationError(errors.New("unsupported access mutation subject"))
	}
}

func ackAccessMutation(ctx context.Context, message accessMutationMessage) {
	if err := message.Ack(); err != nil {
		recordAccessMutationError(ctx, err)
		logAccessMutationFailure(ctx, message, "acknowledgement", err)
		return
	}
	syncAck.Add(1)
	logAccessMutationOutcome(ctx, message, "acknowledged")
}

func terminateAccessMutation(ctx context.Context, message accessMutationMessage, processingErr error) {
	if err := message.Term(); err != nil {
		recordAccessMutationError(ctx, err)
		logAccessMutationFailure(ctx, message, "termination_acknowledgement", err)
		return
	}

	syncTerminal.Add(1)
	logAccessMutationFailure(ctx, message, "terminal", processingErr)
}

func recordAccessMutationError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	errorType := safeErrorType(err)
	span.SetAttributes(attribute.String("error.type", errorType))
	span.SetStatus(codes.Error, errorType)
}

// accessMutationDeliveryAttributes returns the delivery-context log fields
// shared by both the success and failure outcome logs, so an operator can
// correlate a stream sequence and delivery attempt with the object it
// carried regardless of how the delivery attempt concluded.
func accessMutationDeliveryAttributes(message accessMutationMessage) []any {
	attributes := []any{"subject", message.Subject()}
	if metadata, metadataErr := message.Metadata(); metadataErr != nil {
		attributes = append(attributes, "metadata_error", metadataErr)
	} else {
		attributes = append(
			attributes,
			"stream_sequence", metadata.Sequence.Stream,
			"delivery_count", metadata.NumDelivered,
		)
	}

	objectType, uid, objectErr := decodeAccessMutationObject(message.Data())
	attributes = append(attributes, "object_type", objectType, "uid", uid)
	if objectErr != nil {
		attributes = append(attributes, "object_context_error", objectErr)
	}
	return attributes
}

func logAccessMutationFailure(
	ctx context.Context,
	message accessMutationMessage,
	classification string,
	err error,
) {
	attributes := append([]any{
		"error_type", safeErrorType(err),
		"classification", classification,
	}, accessMutationDeliveryAttributes(message)...)
	logger.With(attributes...).ErrorContext(ctx, "access mutation delivery failure")
}

// logAccessMutationOutcome logs a successful delivery outcome (currently just
// "acknowledged") with the same stream sequence, delivery count, and object
// context as logAccessMutationFailure, so successful and failed deliveries
// are equally traceable to a specific tuple sync. Kept at InfoContext (not
// DebugContext) deliberately while the JetStream migration rolls out, to
// make delivery-attempt correlation visible by default; revisit once the
// rollout is proven stable.
func logAccessMutationOutcome(ctx context.Context, message accessMutationMessage, classification string) {
	attributes := append([]any{
		"classification", classification,
	}, accessMutationDeliveryAttributes(message)...)
	logger.With(attributes...).InfoContext(ctx, "access mutation delivery outcome")
}

// maxDeliveryAdvisory is the JetStream $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES
// payload used as the authoritative signal that a message exhausted MaxDeliver.
type maxDeliveryAdvisory struct {
	ID         string `json:"id"`
	Stream     string `json:"stream"`
	Consumer   string `json:"consumer"`
	StreamSeq  uint64 `json:"stream_seq"`
	Deliveries uint64 `json:"deliveries"`
}

// retainedMessageGetter is the subset of [jetstream.Stream] needed to fetch a
// retained message for advisory enrichment.
type retainedMessageGetter interface {
	GetMsg(context.Context, uint64, ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error)
}

// startMaxDeliveryAdvisorySubscription subscribes to max-delivery advisories on
// a best-effort, ephemeral core NATS subscription. Advisories are not
// persisted, so exhaustion detection while fga-sync is disconnected relies on
// external monitoring of sync_max_deliver_exhausted and stream ACK lag.
func startMaxDeliveryAdvisorySubscription(
	ctx context.Context,
	nc *nats.Conn,
	js jetstream.JetStream,
) error {
	stream, err := js.Stream(ctx, constants.FgaSyncStreamName)
	if err != nil {
		return fmt.Errorf("get access mutation stream: %w", err)
	}

	_, err = nc.QueueSubscribe(
		constants.FgaSyncMaxDeliveryAdvisorySubject,
		constants.FgaSyncQueue,
		func(message *nats.Msg) {
			if advisoryErr := handleMaxDeliveryAdvisory(ctx, stream, message.Data); advisoryErr != nil {
				logger.With(errKey, advisoryErr).
					ErrorContext(ctx, "failed to handle max-delivery advisory")
			}
		},
	)
	if err != nil {
		return fmt.Errorf("subscribe to max-delivery advisories: %w", err)
	}

	return nil
}

func handleMaxDeliveryAdvisory(
	ctx context.Context,
	stream retainedMessageGetter,
	payload []byte,
) error {
	var advisory maxDeliveryAdvisory
	if err := json.Unmarshal(payload, &advisory); err != nil {
		return fmt.Errorf("decode max-delivery advisory: %w", err)
	}
	if err := validateMaxDeliveryAdvisory(advisory); err != nil {
		return err
	}

	syncMaxDeliverExhausted.Add(1)
	getMsgCtx, cancel := context.WithTimeout(ctx, accessMutationAdvisoryLookupTimeout)
	defer cancel()
	retainedMessage, err := stream.GetMsg(getMsgCtx, advisory.StreamSeq)
	if err != nil {
		logExhaustedAccessMutation(ctx, advisory, "", "", err)
		return err
	}

	objectType, uid, err := decodeAccessMutationObject(retainedMessage.Data)
	logExhaustedAccessMutation(ctx, advisory, objectType, uid, err)
	return err
}

func validateMaxDeliveryAdvisory(advisory maxDeliveryAdvisory) error {
	switch {
	case advisory.Stream != constants.FgaSyncStreamName:
		return errors.New("max-delivery advisory has unexpected stream")
	case advisory.Consumer != constants.FgaSyncAccessMutationConsumerName:
		return errors.New("max-delivery advisory has unexpected consumer")
	case advisory.StreamSeq == 0:
		return errors.New("max-delivery advisory has no stream sequence")
	default:
		return nil
	}
}

// decodeAccessMutationObject decodes the object type and UID from a raw
// access mutation message payload, for attaching object context to delivery
// outcome logs (success, failure, and max-delivery exhaustion).
func decodeAccessMutationObject(data []byte) (string, string, error) {
	var message fgatypes.GenericFGAMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return "", "", fmt.Errorf("decode retained access mutation: %w", err)
	}

	var objectData struct {
		UID string `json:"uid"`
	}
	if err := message.UnmarshalData(&objectData); err != nil {
		return message.ObjectType, "", fmt.Errorf("decode retained object context: %w", err)
	}
	return message.ObjectType, objectData.UID, nil
}

func logExhaustedAccessMutation(
	ctx context.Context,
	advisory maxDeliveryAdvisory,
	objectType string,
	uid string,
	enrichmentErr error,
) {
	logger.With(
		"advisory_id", advisory.ID,
		"stream_sequence", advisory.StreamSeq,
		"deliveries", advisory.Deliveries,
		"object_type", objectType,
		"uid", uid,
		"enrichment_error", enrichmentErr,
	).ErrorContext(ctx, "access mutation exhausted max deliveries")
}
