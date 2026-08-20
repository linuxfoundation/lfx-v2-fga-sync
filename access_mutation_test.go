// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type testAccessMutationMessage struct {
	data        []byte
	headers     nats.Header
	subject     string
	metadata    *jetstream.MsgMetadata
	metadataErr error
	ackErr      error
	termErr     error
	ackCalls    int
	termCalls   int
}

type testRetainedMessageGetter struct {
	message *jetstream.RawStreamMsg
	err     error
	seq     uint64
}

type testAccessMutationConsumeContext struct {
	closed <-chan struct{}
	events *[]string
}

func (c *testAccessMutationConsumeContext) Stop() {
	*c.events = append(*c.events, "stop")
}

func (c *testAccessMutationConsumeContext) Closed() <-chan struct{} {
	return c.closed
}

func (g *testRetainedMessageGetter) GetMsg(
	_ context.Context,
	seq uint64,
	_ ...jetstream.GetMsgOpt,
) (*jetstream.RawStreamMsg, error) {
	g.seq = seq
	return g.message, g.err
}

func (m *testAccessMutationMessage) Data() []byte {
	return m.data
}

func (m *testAccessMutationMessage) Headers() nats.Header {
	return m.headers
}

func (m *testAccessMutationMessage) Subject() string {
	return m.subject
}

func (m *testAccessMutationMessage) Metadata() (*jetstream.MsgMetadata, error) {
	if m.metadataErr != nil {
		return nil, m.metadataErr
	}
	if m.metadata != nil {
		return m.metadata, nil
	}
	return &jetstream.MsgMetadata{}, nil
}

func (m *testAccessMutationMessage) Ack() error {
	m.ackCalls++
	return m.ackErr
}

func (m *testAccessMutationMessage) Term() error {
	m.termCalls++
	return m.termErr
}

func TestAccessMutationConsumerConfig(t *testing.T) {
	t.Parallel()

	config := accessMutationConsumerConfig()

	assert.Equal(t, constants.FgaSyncAccessMutationConsumerName, config.Name)
	assert.Equal(t, constants.FgaSyncAccessMutationConsumerName, config.Durable)
	assert.Equal(t, jetstream.DeliverNewPolicy, config.DeliverPolicy)
	assert.Equal(t, jetstream.AckExplicitPolicy, config.AckPolicy)
	assert.Equal(t, 1, config.MaxAckPending)
	assert.Equal(t, 7, config.MaxDeliver)
	assert.Zero(t, config.AckWait)
	assert.Equal(t, []time.Duration{
		2 * time.Minute,
		2 * time.Minute,
		5 * time.Minute,
		10 * time.Minute,
		15 * time.Minute,
		30 * time.Minute,
	}, config.BackOff)
	assert.Equal(t, []string{
		constants.GenericUpdateAccessSubject,
		constants.GenericDeleteAccessSubject,
		constants.GenericMemberPutSubject,
		constants.GenericMemberRemoveSubject,
	}, config.FilterSubjects)
}

func TestAccessMutationAttemptContextUsesNinetySecondDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := accessMutationAttemptContext(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, time.Now().Add(90*time.Second), deadline, time.Second)
}

func TestProcessAccessMutationMessageOutcomes(t *testing.T) {
	tests := []struct {
		name           string
		payload        string
		subject        string
		fgaErr         error
		ackErr         error
		termErr        error
		wantAckCalls   int
		wantTermCalls  int
		ackDelta       int64
		transientDelta int64
		terminalDelta  int64
	}{
		{
			name:         "success ACKs",
			payload:      `{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`,
			subject:      constants.GenericUpdateAccessSubject,
			wantAckCalls: 1,
			ackDelta:     1,
		},
		{
			name:         "ACK failure stays unacknowledged",
			payload:      `{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`,
			subject:      constants.GenericUpdateAccessSubject,
			ackErr:       errors.New("ack failed"),
			wantAckCalls: 1,
		},
		{
			name:          "terminal payload terminates",
			payload:       `{`,
			subject:       constants.GenericDeleteAccessSubject,
			wantTermCalls: 1,
			terminalDelta: 1,
		},
		{
			name:          "TERM failure stays unacknowledged",
			payload:       `{`,
			subject:       constants.GenericDeleteAccessSubject,
			termErr:       errors.New("term failed"),
			wantTermCalls: 1,
		},
		{
			name:           "FGA failure stays transient",
			payload:        `{"object_type":"committee","operation":"delete_access","data":{"uid":"resource-1"}}`,
			subject:        constants.GenericDeleteAccessSubject,
			fgaErr:         assert.AnError,
			transientDelta: 1,
		},
		{
			name: "member_put dispatches to its handler and ACKs",
			payload: `{"object_type":"committee","operation":"member_put",` +
				`"data":{"uid":"resource-1","username":"user-1","relations":["member"]}}`,
			subject:      constants.GenericMemberPutSubject,
			wantAckCalls: 1,
			ackDelta:     1,
		},
		{
			name:         "member_remove dispatches to its handler and ACKs",
			payload:      `{"object_type":"committee","operation":"member_remove","data":{"uid":"resource-1","username":"user-1"}}`,
			subject:      constants.GenericMemberRemoveSubject,
			wantAckCalls: 1,
			ackDelta:     1,
		},
		{
			name:          "member_put malformed payload terminates",
			payload:       `{`,
			subject:       constants.GenericMemberPutSubject,
			wantTermCalls: 1,
			terminalDelta: 1,
		},
		{
			name:          "member_remove malformed payload terminates",
			payload:       `{`,
			subject:       constants.GenericMemberRemoveSubject,
			wantTermCalls: 1,
			terminalDelta: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := setupService()
			fgaClient := service.fgaService.client.(*MockFgaClient)
			fgaClient.
				On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
				Return(&client.ClientReadResponse{}, tt.fgaErr)
			fgaClient.
				On("Write", mock.Anything, mock.Anything, mock.Anything).
				Return(&client.ClientWriteResponse{}, nil)

			message := &testAccessMutationMessage{
				data:    []byte(tt.payload),
				subject: tt.subject,
				ackErr:  tt.ackErr,
				termErr: tt.termErr,
			}
			ackBefore := syncAck.Value()
			transientBefore := syncTransientAttempts.Value()
			terminalBefore := syncTerminal.Value()

			processAccessMutationMessage(context.Background(), service, message)

			assert.Equal(t, tt.wantAckCalls, message.ackCalls)
			assert.Equal(t, tt.wantTermCalls, message.termCalls)
			assert.Equal(t, tt.ackDelta, syncAck.Value()-ackBefore)
			assert.Equal(t, tt.transientDelta, syncTransientAttempts.Value()-transientBefore)
			assert.Equal(t, tt.terminalDelta, syncTerminal.Value()-terminalBefore)
		})
	}
}

// TestProcessAccessMutationMessageFgaErrorsStayTransient does not run its
// subtests in parallel: they assert on deltas to the package-level expvar
// counters, which would race against each other under t.Parallel().
func TestProcessAccessMutationMessageFgaErrorsStayTransient(t *testing.T) {
	fgaErrors := []struct {
		name string
		err  error
	}{
		{name: "unknown SDK error", err: assert.AnError},
		{
			name: "connection refused",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		},
		{name: "context deadline", err: context.DeadlineExceeded},
		{name: "propagated validation error", err: makeValidationError("request validation failed")},
		{name: "HTTP 400", err: fakeStatusErr{code: 400}},
		{name: "HTTP 401", err: fakeStatusErr{code: 401}},
		{name: "HTTP 403", err: fakeStatusErr{code: 403}},
		{name: "HTTP 408", err: fakeStatusErr{code: 408}},
		{name: "HTTP 409", err: fakeStatusErr{code: 409}},
		{name: "HTTP 429", err: fakeStatusErr{code: 429}},
		{name: "HTTP 500", err: fakeStatusErr{code: 500}},
	}

	for _, tt := range fgaErrors {
		t.Run(tt.name, func(t *testing.T) {
			service := setupService()
			service.fgaService.client.(*MockFgaClient).
				On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
				Return((*client.ClientReadResponse)(nil), tt.err)

			message := &testAccessMutationMessage{
				data:    []byte(`{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`),
				subject: constants.GenericUpdateAccessSubject,
			}
			transientBefore := syncTransientAttempts.Value()

			processAccessMutationMessage(context.Background(), service, message)

			assert.Zero(t, message.ackCalls, "must not ACK on an FGA error")
			assert.Zero(t, message.termCalls, "must not TERM on an FGA error")
			assert.Equal(t, int64(1), syncTransientAttempts.Value()-transientBefore)
		})
	}
}

// TestProcessAccessMutationMessagePersistentErrorStaysTransientAcrossRedeliveries
// confirms that a message whose OpenFGA write repeatedly fails with a
// propagated validation error is never ACKed or TERMed on any delivery
// attempt. Combined with the pinned consumer config (MaxAckPending: 1,
// MaxDeliver: 7), this is what makes such a message occupy the single
// global in-flight slot for the full BackOff schedule rather than being
// terminated early or displaced by another message.
func TestProcessAccessMutationMessagePersistentErrorStaysTransientAcrossRedeliveries(t *testing.T) {
	service := setupService()
	service.fgaService.client.(*MockFgaClient).
		On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
		Return((*client.ClientReadResponse)(nil), makeValidationError("request validation failed"))

	config := accessMutationConsumerConfig()
	transientBefore := syncTransientAttempts.Value()

	for attempt := 1; attempt <= config.MaxDeliver; attempt++ {
		message := &testAccessMutationMessage{
			data:    []byte(`{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`),
			subject: constants.GenericUpdateAccessSubject,
		}

		processAccessMutationMessage(context.Background(), service, message)

		assert.Zero(t, message.ackCalls, "attempt %d must not ACK", attempt)
		assert.Zero(t, message.termCalls, "attempt %d must not TERM", attempt)
	}

	assert.Equal(t, int64(config.MaxDeliver), syncTransientAttempts.Value()-transientBefore,
		"every delivery attempt up to MaxDeliver must be counted transient")
}

func TestProcessAccessMutationMessageAcksCacheInvalidationWarning(t *testing.T) {
	service := setupService()
	fgaClient := service.fgaService.client.(*MockFgaClient)
	fgaClient.
		On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
		Return(&client.ClientReadResponse{}, nil)
	fgaClient.
		On("Write", mock.Anything, mock.Anything, mock.Anything).
		Return(&client.ClientWriteResponse{}, nil)
	service.fgaService.cacheBucket.(*MockKeyValue).SetError(assert.AnError)
	message := &testAccessMutationMessage{
		data: []byte(
			`{"object_type":"committee","operation":"update_access",` +
				`"data":{"uid":"resource-1","public":true}}`,
		),
		subject: constants.GenericUpdateAccessSubject,
	}
	before := syncAck.Value()

	processAccessMutationMessage(context.Background(), service, message)

	assert.Equal(t, 1, message.ackCalls)
	assert.Equal(t, int64(1), syncAck.Value()-before)
}

func TestStopAccessMutationConsumerAwaitsInFlightAttemptWithinGrace(t *testing.T) {
	restoreGrace := setAccessMutationShutdownGrace(time.Minute)
	defer restoreGrace()

	events := make([]string, 0, 2)
	closed := make(chan struct{})
	consumer := &testAccessMutationConsumeContext{closed: closed, events: &events}

	// Simulate an in-flight delivery attempt finishing on its own, well
	// within the grace period, without needing a forced cancellation. Only
	// the channel itself is touched from this goroutine so the two
	// goroutines synchronize solely through the channel close/receive.
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(closed)
	}()

	cancelCalls := 0
	start := time.Now()
	stopAccessMutationConsumer(consumer, func() {
		cancelCalls++
		events = append(events, "cancel")
	}, time.Now().Add(time.Minute))
	elapsed := time.Since(start)

	assert.Equal(t, []string{"stop", "cancel"}, events)
	assert.Equal(t, 1, cancelCalls, "cancel must run exactly once, after the attempt finished on its own")
	assert.Less(t, elapsed, 500*time.Millisecond, "must not wait out the full grace period when Closed() fires promptly")
}

func TestStopAccessMutationConsumerForceCancelsAfterGraceTimeout(t *testing.T) {
	restoreGrace := setAccessMutationShutdownGrace(10 * time.Millisecond)
	defer restoreGrace()

	events := make([]string, 0, 3)
	closed := make(chan struct{})
	consumer := &testAccessMutationConsumeContext{closed: closed, events: &events}

	cancelCalls := 0
	stopAccessMutationConsumer(consumer, func() {
		cancelCalls++
		events = append(events, "cancel")
		// Simulate cancellation aborting the stuck in-flight attempt,
		// letting the consume loop finally close.
		if cancelCalls == 1 {
			close(closed)
		}
	}, time.Now().Add(time.Minute))

	assert.Equal(t, []string{"stop", "cancel"}, events)
	assert.Equal(t, 1, cancelCalls, "cancel must run exactly once, to force the stuck attempt to abort")
}

// TestStopAccessMutationConsumerBoundedByDeadlineEvenAfterCancel confirms
// that a consumer whose Closed() channel never fires -- even after
// cancellation -- does not block shutdown forever. Both the pre-cancel grace
// wait and the post-cancel wait must be bounded by the shared deadline.
func TestStopAccessMutationConsumerBoundedByDeadlineEvenAfterCancel(t *testing.T) {
	restoreGrace := setAccessMutationShutdownGrace(time.Minute)
	defer restoreGrace()

	events := make([]string, 0, 2)
	closed := make(chan struct{})
	consumer := &testAccessMutationConsumeContext{closed: closed, events: &events}
	// closed is intentionally never closed, simulating a consumer stuck even
	// after cancellation.

	cancelCalls := 0
	start := time.Now()
	stopAccessMutationConsumer(consumer, func() {
		cancelCalls++
		events = append(events, "cancel")
	}, time.Now().Add(50*time.Millisecond))
	elapsed := time.Since(start)

	assert.Equal(t, 1, cancelCalls, "cancel must still run once the deadline forces the grace wait to give up")
	assert.Less(t, elapsed, 500*time.Millisecond, "must give up once the deadline passes rather than blocking on Closed() forever")
}

// setAccessMutationShutdownGrace overrides the package-level shutdown grace
// for a single test and returns a function that restores the original value.
func setAccessMutationShutdownGrace(d time.Duration) func() {
	original := accessMutationShutdownGrace
	accessMutationShutdownGrace = d
	return func() { accessMutationShutdownGrace = original }
}

func setAccessMutationRecoveryInterval(d time.Duration) func() {
	original := accessMutationRecoveryInterval
	accessMutationRecoveryInterval = d
	return func() { accessMutationRecoveryInterval = original }
}

func TestCoreSubscriptionsExcludeMigratedAccessSubjects(t *testing.T) {
	t.Parallel()

	configs := queueSubscriptionConfigs(*setupService())
	subjects := make(map[string]bool, len(configs))
	for _, config := range configs {
		subjects[config.subject] = true
	}

	assert.False(t, subjects[constants.GenericUpdateAccessSubject])
	assert.False(t, subjects[constants.GenericDeleteAccessSubject])
	assert.False(t, subjects[constants.GenericMemberPutSubject])
	assert.False(t, subjects[constants.GenericMemberRemoveSubject])
	assert.True(t, subjects[constants.AccessCheckSubject])
	assert.True(t, subjects[constants.ReadTuplesSubject])
	assert.Len(t, subjects, 2)
}

func TestHandleMaxDeliveryAdvisoryEnrichesObjectContext(t *testing.T) {
	getter := &testRetainedMessageGetter{
		message: &jetstream.RawStreamMsg{
			Subject: constants.GenericUpdateAccessSubject,
			Data: []byte(
				`{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`,
			),
		},
	}
	advisory := []byte(
		`{"id":"event-1","stream":"fga-sync-events","consumer":"fga-sync-access-mutation-consumer",` +
			`"stream_seq":42,"deliveries":7}`,
	)
	before := syncMaxDeliverExhausted.Value()

	err := handleMaxDeliveryAdvisory(context.Background(), getter, advisory)

	require.NoError(t, err)
	assert.Equal(t, uint64(42), getter.seq)
	assert.Equal(t, int64(1), syncMaxDeliverExhausted.Value()-before)
}

func TestHandleMaxDeliveryAdvisoryCountsEnrichmentFailure(t *testing.T) {
	getter := &testRetainedMessageGetter{err: assert.AnError}
	advisory := []byte(
		`{"id":"event-2","stream":"fga-sync-events","consumer":"fga-sync-access-mutation-consumer",` +
			`"stream_seq":43,"deliveries":7}`,
	)
	before := syncMaxDeliverExhausted.Value()

	err := handleMaxDeliveryAdvisory(context.Background(), getter, advisory)

	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, int64(1), syncMaxDeliverExhausted.Value()-before)
}

func TestHandleMaxDeliveryAdvisoryRejectsMalformedPayload(t *testing.T) {
	getter := &testRetainedMessageGetter{}
	before := syncMaxDeliverExhausted.Value()

	err := handleMaxDeliveryAdvisory(context.Background(), getter, []byte(`{`))

	require.Error(t, err)
	assert.Zero(t, syncMaxDeliverExhausted.Value()-before)
	assert.Zero(t, getter.seq)
}

// attributesToMap converts the flat key/value slice passed to logger.With
// into a map, so tests can assert on individual fields regardless of order.
func attributesToMap(t *testing.T, attributes []any) map[string]any {
	t.Helper()
	require.Zero(t, len(attributes)%2, "attributes must be an even number of key/value entries")

	result := make(map[string]any, len(attributes)/2)
	for i := 0; i < len(attributes); i += 2 {
		key, ok := attributes[i].(string)
		require.True(t, ok, "attribute key at index %d must be a string", i)
		result[key] = attributes[i+1]
	}
	return result
}

// TestAccessMutationDeliveryAttributesHappyPath covers the fields both
// logAccessMutationOutcome (success) and logAccessMutationFailure (failure)
// rely on to correlate a delivery attempt with the object it carried.
func TestAccessMutationDeliveryAttributesHappyPath(t *testing.T) {
	t.Parallel()

	message := &testAccessMutationMessage{
		data: []byte(
			`{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`,
		),
		subject: constants.GenericUpdateAccessSubject,
		metadata: &jetstream.MsgMetadata{
			Sequence:     jetstream.SequencePair{Stream: 42},
			NumDelivered: 3,
		},
	}

	attributes := attributesToMap(t, accessMutationDeliveryAttributes(message))

	assert.Equal(t, constants.GenericUpdateAccessSubject, attributes["subject"])
	assert.Equal(t, uint64(42), attributes["stream_sequence"])
	assert.Equal(t, uint64(3), attributes["delivery_count"])
	assert.Equal(t, "committee", attributes["object_type"])
	assert.Equal(t, "resource-1", attributes["uid"])
	assert.NotContains(t, attributes, "metadata_error")
	assert.NotContains(t, attributes, "object_context_error")
}

// TestAccessMutationDeliveryAttributesMetadataError confirms a
// Metadata() failure is surfaced instead of a stream sequence and delivery
// count, rather than silently omitting delivery context or panicking on a
// nil metadata dereference.
func TestAccessMutationDeliveryAttributesMetadataError(t *testing.T) {
	t.Parallel()

	message := &testAccessMutationMessage{
		data: []byte(
			`{"object_type":"committee","operation":"update_access","data":{"uid":"resource-1"}}`,
		),
		subject:     constants.GenericUpdateAccessSubject,
		metadataErr: assert.AnError,
	}

	attributes := attributesToMap(t, accessMutationDeliveryAttributes(message))

	assert.Equal(t, assert.AnError, attributes["metadata_error"])
	assert.NotContains(t, attributes, "stream_sequence")
	assert.NotContains(t, attributes, "delivery_count")
	assert.Equal(t, "committee", attributes["object_type"])
	assert.Equal(t, "resource-1", attributes["uid"])
}

// TestAccessMutationDeliveryAttributesMalformedPayload confirms a payload
// that cannot be decoded reports an object_context_error with empty object
// fields rather than the delivery attempt's real (but undecodable) context.
func TestAccessMutationDeliveryAttributesMalformedPayload(t *testing.T) {
	t.Parallel()

	message := &testAccessMutationMessage{
		data:    []byte(`{`),
		subject: constants.GenericDeleteAccessSubject,
	}

	attributes := attributesToMap(t, accessMutationDeliveryAttributes(message))

	assert.Equal(t, "", attributes["object_type"])
	assert.Equal(t, "", attributes["uid"])
	require.Contains(t, attributes, "object_context_error")
	assert.Error(t, attributes["object_context_error"].(error))
}

type testManagedConsumeContext struct {
	closed chan struct{}
	once   sync.Once
}

func newTestManagedConsumeContext(closed bool) *testManagedConsumeContext {
	ctx := &testManagedConsumeContext{closed: make(chan struct{})}
	if closed {
		ctx.once.Do(func() { close(ctx.closed) })
	}
	return ctx
}

func (c *testManagedConsumeContext) Stop() {
	c.once.Do(func() { close(c.closed) })
}

func (c *testManagedConsumeContext) Drain() {
	c.Stop()
}

func (c *testManagedConsumeContext) Closed() <-chan struct{} {
	return c.closed
}

type testManagedConsumer struct {
	jetstream.Consumer
	consumeContext jetstream.ConsumeContext
}

func (c *testManagedConsumer) Consume(
	_ jetstream.MessageHandler,
	_ ...jetstream.PullConsumeOpt,
) (jetstream.ConsumeContext, error) {
	return c.consumeContext, nil
}

type testAccessMutationConsumerFactory struct {
	jetstream.JetStream
	mu        sync.Mutex
	consumers []jetstream.Consumer
	errs      []error
	calls     chan int
	callCount int
	blockCall map[int]<-chan struct{}
}

func (f *testAccessMutationConsumerFactory) CreateOrUpdateConsumer(
	_ context.Context,
	_ string,
	_ jetstream.ConsumerConfig,
) (jetstream.Consumer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.calls <- f.callCount
	if block := f.blockCall[f.callCount]; block != nil {
		<-block
	}
	if err := f.errs[f.callCount-1]; err != nil {
		return nil, err
	}
	return f.consumers[f.callCount-1], nil
}

func TestAccessMutationConsumerManagerRecoversDeletedConsumer(t *testing.T) {
	restoreInterval := setAccessMutationRecoveryInterval(time.Millisecond)
	defer restoreInterval()

	first := newTestManagedConsumeContext(true)
	second := newTestManagedConsumeContext(false)
	factory := &testAccessMutationConsumerFactory{
		consumers: []jetstream.Consumer{
			&testManagedConsumer{consumeContext: first},
			&testManagedConsumer{consumeContext: second},
		},
		errs:  make([]error, 2),
		calls: make(chan int, 2),
	}

	consumer, err := startAccessMutationConsumer(context.Background(), factory, HandlerService{})
	require.NoError(t, err)
	assert.Equal(t, 1, <-factory.calls)

	manager := consumer.(*accessMutationConsumerManager)
	manager.handleConsumeError(context.Background(), jetstream.ErrConsumerDeleted)

	select {
	case call := <-factory.calls:
		assert.Equal(t, 2, call)
	case <-time.After(time.Second):
		t.Fatal("consumer was not recreated after ErrConsumerDeleted")
	}

	consumer.Stop()
	select {
	case <-consumer.Closed():
	case <-time.After(time.Second):
		t.Fatal("consumer manager did not stop")
	}
}

func TestAccessMutationConsumerManagerCoalescesRecoverySignals(t *testing.T) {
	manager := &accessMutationConsumerManager{recover: make(chan struct{}, 1)}

	manager.requestRecovery(1)
	manager.requestRecovery(1)

	assert.Len(t, manager.recover, 1)
}

func TestAccessMutationConsumerManagerIgnoresStaleRecoverySignal(t *testing.T) {
	first := newTestManagedConsumeContext(true)
	second := newTestManagedConsumeContext(false)
	factory := &testAccessMutationConsumerFactory{
		consumers: []jetstream.Consumer{
			&testManagedConsumer{consumeContext: first},
			&testManagedConsumer{consumeContext: second},
		},
		errs:  make([]error, 2),
		calls: make(chan int, 3),
	}

	consumer, err := startAccessMutationConsumer(context.Background(), factory, HandlerService{})
	require.NoError(t, err)
	assert.Equal(t, 1, <-factory.calls)
	manager := consumer.(*accessMutationConsumerManager)

	manager.handleConsumeErrorForGeneration(
		context.Background(),
		jetstream.ErrConsumerDeleted,
		1,
	)
	assert.Equal(t, 2, <-factory.calls)

	manager.handleConsumeErrorForGeneration(
		context.Background(),
		jetstream.ErrConsumerDeleted,
		1,
	)
	select {
	case call := <-factory.calls:
		t.Fatalf("stale deletion signal triggered recreation call %d", call)
	case <-time.After(10 * time.Millisecond):
	}

	consumer.Stop()
	<-consumer.Closed()
}

func TestAccessMutationConsumerManagerDoesNotRecreateAfterStop(t *testing.T) {
	for range 100 {
		current := newTestManagedConsumeContext(true)
		stop := make(chan struct{})
		close(stop)
		factory := &testAccessMutationConsumerFactory{
			consumers: []jetstream.Consumer{
				&testManagedConsumer{consumeContext: newTestManagedConsumeContext(false)},
			},
			errs:  []error{nil},
			calls: make(chan int, 1),
		}
		manager := &accessMutationConsumerManager{
			ctx:     context.Background(),
			factory: factory,
			current: current,
			stop:    stop,
		}

		assert.False(t, manager.recoverCurrent())
		select {
		case call := <-factory.calls:
			t.Fatalf("shutdown triggered recreation call %d", call)
		default:
		}
	}
}

func TestAccessMutationConsumerManagerStopsCreationAlreadyInFlight(t *testing.T) {
	first := newTestManagedConsumeContext(true)
	second := newTestManagedConsumeContext(false)
	releaseCreate := make(chan struct{})
	factory := &testAccessMutationConsumerFactory{
		consumers: []jetstream.Consumer{
			&testManagedConsumer{consumeContext: first},
			&testManagedConsumer{consumeContext: second},
		},
		errs:      make([]error, 2),
		calls:     make(chan int, 2),
		blockCall: map[int]<-chan struct{}{2: releaseCreate},
	}

	consumer, err := startAccessMutationConsumer(context.Background(), factory, HandlerService{})
	require.NoError(t, err)
	assert.Equal(t, 1, <-factory.calls)
	consumer.(*accessMutationConsumerManager).
		handleConsumeError(context.Background(), jetstream.ErrConsumerDeleted)
	assert.Equal(t, 2, <-factory.calls)

	consumer.Stop()
	close(releaseCreate)

	select {
	case <-consumer.Closed():
	case <-time.After(time.Second):
		t.Fatal("consumer manager did not stop an in-flight recreation")
	}
	select {
	case <-second.Closed():
	default:
		t.Fatal("consume context created during shutdown was not stopped")
	}
}

func TestAccessMutationConsumerManagerRetriesRecreation(t *testing.T) {
	restoreInterval := setAccessMutationRecoveryInterval(time.Millisecond)
	defer restoreInterval()

	first := newTestManagedConsumeContext(true)
	second := newTestManagedConsumeContext(false)
	factory := &testAccessMutationConsumerFactory{
		consumers: []jetstream.Consumer{
			&testManagedConsumer{consumeContext: first},
			nil,
			&testManagedConsumer{consumeContext: second},
		},
		errs:  []error{nil, assert.AnError, nil},
		calls: make(chan int, 3),
	}

	consumer, err := startAccessMutationConsumer(context.Background(), factory, HandlerService{})
	require.NoError(t, err)
	assert.Equal(t, 1, <-factory.calls)

	consumer.(*accessMutationConsumerManager).
		handleConsumeError(context.Background(), jetstream.ErrConsumerDeleted)

	assert.Equal(t, 2, <-factory.calls)
	select {
	case call := <-factory.calls:
		assert.Equal(t, 3, call)
	case <-time.After(time.Second):
		t.Fatal("consumer recreation was not retried")
	}

	consumer.Stop()
	<-consumer.Closed()
}

func TestAccessMutationConsumerManagerDoesNotRecoverOtherErrors(t *testing.T) {
	current := newTestManagedConsumeContext(false)
	factory := &testAccessMutationConsumerFactory{
		consumers: []jetstream.Consumer{
			&testManagedConsumer{consumeContext: current},
		},
		errs:  []error{nil},
		calls: make(chan int, 2),
	}

	consumer, err := startAccessMutationConsumer(context.Background(), factory, HandlerService{})
	require.NoError(t, err)
	assert.Equal(t, 1, <-factory.calls)

	consumer.(*accessMutationConsumerManager).
		handleConsumeError(context.Background(), jetstream.ErrNoHeartbeat)

	select {
	case call := <-factory.calls:
		t.Fatalf("unexpected consumer recreation call %d", call)
	case <-time.After(10 * time.Millisecond):
	}

	consumer.Stop()
	<-consumer.Closed()
}

func TestAccessMutationConsumerManagerStopsDuringRecovery(t *testing.T) {
	restoreInterval := setAccessMutationRecoveryInterval(time.Minute)
	defer restoreInterval()

	first := newTestManagedConsumeContext(true)
	factory := &testAccessMutationConsumerFactory{
		consumers: []jetstream.Consumer{
			&testManagedConsumer{consumeContext: first},
			nil,
		},
		errs:  []error{nil, assert.AnError},
		calls: make(chan int, 3),
	}

	consumer, err := startAccessMutationConsumer(context.Background(), factory, HandlerService{})
	require.NoError(t, err)
	assert.Equal(t, 1, <-factory.calls)

	consumer.(*accessMutationConsumerManager).
		handleConsumeError(context.Background(), jetstream.ErrConsumerDeleted)
	assert.Equal(t, 2, <-factory.calls)

	consumer.Stop()
	select {
	case <-consumer.Closed():
	case <-time.After(time.Second):
		t.Fatal("consumer manager did not stop while waiting to retry")
	}

	select {
	case call := <-factory.calls:
		t.Fatalf("unexpected recreation call %d after stop", call)
	default:
	}
}
