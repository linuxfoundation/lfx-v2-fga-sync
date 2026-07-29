# fga-sync-message-delivery

Durable delivery and acknowledgement of access-sync messages. Subject, envelope,
and reply semantics are governed by `docs/fga-sync-contract.md`; this capability
defines how fga-sync receives, retries, and acknowledges those messages.

## ADDED Requirements

### Requirement: Update and delete access are asynchronous-only

Publishers SHALL use core NATS `Publish` for
`lfx.fga-sync.update_access` and `lfx.fga-sync.delete_access` and SHALL NOT use
request/reply for OpenFGA convergence. The subjects and `GenericFGAMessage`
envelopes SHALL remain unchanged. fga-sync SHALL NOT provide an application
`"OK"` reply for either subject. Expanded project-service LFXV2-2830 and
committee-service LFXV2-2828 SHALL be deployed, and all other publishers SHALL
be verified asynchronous-only, before the stream starts capturing either
subject.

The JetStream `INatsMsg` adapter SHALL return an empty application `Reply()` and
SHALL NOT expose or respond to the JetStream ACK reply subject.

#### Scenario: HTTP X-Sync does not wait for OpenFGA

- **WHEN** a project or committee mutation is requested with `X-Sync: true`
- **THEN** applicable indexer request/reply behavior remains unchanged
- **AND** its FGA update or delete message is published asynchronously
- **AND** the HTTP response does not claim that OpenFGA has converged

### Requirement: Durable delivery of update and delete access messages

The service SHALL consume `lfx.fga-sync.update_access` and
`lfx.fga-sync.delete_access` through shared durable pull consumer
`fga-sync-access-mutation-consumer`, bound to stream `fga-sync-events`. The
stream SHALL persist both subjects in one order using limits retention,
`maxAge: 24h`, file storage, and three replicas. The consumer SHALL use
`DeliverNewPolicy`, explicit acknowledgement, `MaxAckPending: 1`,
`MaxDeliver: 7`, and consumer `BackOff` values of `2m`, `2m`, `5m`, `10m`,
`15m`, and `30m`.

`DeliverNewPolicy` SHALL apply when the durable is first created. The initial
cutover SHALL create it only after the stream is verified empty. If the durable
already exists, service restarts SHALL bind its stored cursor and resume
pending work. If the durable state is missing, the service SHALL recreate it at
the current stream tail without replaying or purging retained history. New
messages SHALL then process without manual intervention or blocking unrelated
access-check/core-subscription handlers. Retained messages predating recreation
SHALL age out under the stream's 24-hour retention and MAY require fresh
authoritative-state reconciliation.

If a running `Consume()` loop reports terminal
`jetstream.ErrConsumerDeleted`, the service SHALL keep the process and
unrelated handlers running, SHALL coalesce repeated local recovery signals,
and SHALL retry `CreateOrUpdateConsumer` every two seconds until the shared
durable is recreated or rebound and local consumption restarts. Recovery SHALL
use the same `DeliverNewPolicy` configuration and SHALL NOT purge the stream.
Graceful shutdown SHALL stop the recovery loop and whichever consume context is
current.

The service SHALL NOT also consume either migrated subject through a core NATS
`QueueSubscribe`. `member_put`, `member_remove`, `access_check.request`, and
`read_tuples` SHALL remain on core NATS in Phase 1. A subscription-wiring test
SHALL verify both migrated subjects are absent from the core subscription set.

#### Scenario: Access mutation delivered via JetStream

- **WHEN** a publisher publishes a valid update or delete access message
- **THEN** the message is stored in the fga-sync stream
- **AND** the durable consumer delivers it to a single service instance
- **AND** the message is not also delivered via a core NATS queue subscription

#### Scenario: Access mutation received while an instance was offline

- **GIVEN** the durable consumer already exists
- **WHEN** no service instance is running at publish time
- **AND** an instance later starts and binds the durable consumer
- **THEN** the previously published message is delivered rather than lost

#### Scenario: Durable consumer state is lost

- **WHEN** the durable consumer is missing while the limits-retention stream
  still contains prior messages
- **THEN** the service recreates the consumer with `DeliverNewPolicy`
- **AND** delivery begins with messages published after consumer recreation
- **AND** retained pre-recreation history is not replayed or purged
- **AND** unrelated access-check and core-subscription processing is not
  blocked waiting for manual recovery
- **AND** skipped retained history expires under `maxAge`

#### Scenario: Durable is deleted while replicas are running

- **WHEN** a running consume loop reports `jetstream.ErrConsumerDeleted`
- **THEN** the process and unrelated handlers remain available
- **AND** the replica retries durable creation/binding every two seconds
- **AND** its consume loop restarts after the shared durable is available
- **AND** concurrent replicas do not purge the stream

### Requirement: Redelivery on transient processing failure

The service SHALL leave a migrated access message unacknowledged when
processing returns any error not marked with the terminal-validation sentinel.
The wrapper SHALL NOT call `Nak()` or `NakWithDelay()`. JetStream SHALL
redeliver the same pending message using the consumer `BackOff` schedule.
Unmarked errors SHALL include connection refused, transport failures, context
deadline, unknown SDK errors, HTTP 408, 409, 429, 5xx, and operational 401/403
configuration failures. A blanket HTTP 4xx predicate SHALL NOT classify a
message as terminal.

#### Scenario: OpenFGA temporarily unavailable

- **WHEN** an update or delete access message is processed while OpenFGA is
  unreachable
- **THEN** the service leaves the message unacknowledged
- **AND** JetStream redelivers it after a backoff delay
- **AND** when OpenFGA becomes reachable the tuple is written and the message is
  acknowledged

#### Scenario: No-status failures remain transient

- **WHEN** OpenFGA returns connection refused, an unknown no-status SDK error,
  or the 90-second context deadline expires
- **THEN** the error is not marked terminal
- **AND** the message remains unacknowledged for JetStream redelivery

#### Scenario: Transient failure exceeds max delivery

- **WHEN** a migrated access message fails transiently on every delivery attempt
- **AND** the maximum delivery count is reached
- **THEN** the service stops receiving further redeliveries of that message
- **AND** the message remains in the stream until stream retention removes it
- **AND** the JetStream max-delivery advisory is treated as the authoritative
  exhaustion signal
- **AND** a max-delivery-exhausted metric and structured log are emitted

#### Scenario: OpenFGA remains unavailable beyond retention

- **WHEN** OpenFGA remains unavailable or operationally misconfigured while
  access mutations continue to be published
- **THEN** each head message follows the complete bounded retry schedule before
  the next sequence can advance
- **AND** max-delivery advisories and consumer lag expose the failure
- **AND** messages that remain stored longer than the 24-hour stream limit may
  expire without being applied
- **AND** recovery uses fresh authoritative state rather than blindly replaying
  an expired or exhausted snapshot

### Requirement: Terminal messages are terminated without retry

The service SHALL terminate proven local payload/schema failures without retry.
Existing validation returns in `genericUpdateAccessHandler`
and `genericDeleteAccessHandler` (`handler_generic.go`), plus
`processStandardAccessUpdate` (`handler.go`), SHALL wrap those failures with one
terminal-validation sentinel. The wrapper SHALL NOT duplicate payload
validation. The service SHALL call `Term()` only when processing returns that
sentinel. Terminal failures include malformed JSON, missing required fields,
wrong operation, locally detected invalid reference format, and proven
non-recoverable local payload/schema validation. The existing `FgaService`
handling of a recognized OpenFGA invalid tuple SHALL remain unchanged: remove
that tuple and continue the remaining batch. Any other OpenFGA validation error
that propagates from `FgaService` SHALL remain unmarked and transient. A blanket
HTTP 400 predicate SHALL NOT make an error terminal.

Every unmarked error SHALL default to transient.

The service SHALL increment `sync_ack` and `sync_terminal` only after `Ack()` or
`Term()` returns nil. If either acknowledgement call fails, the service SHALL
log and trace the failure and SHALL leave the message unacknowledged without
issuing a fallback NAK.

#### Scenario: Malformed payload

- **WHEN** an update or delete access message contains malformed JSON or is
  missing a required field
- **THEN** the service terminates the message without writing any tuple
- **AND** the message is not redelivered
- **AND** the terminated message remains in the limits stream until its 24-hour
  retention expires
- **AND** a terminal-error metric is incremented

#### Scenario: Write succeeds but cache invalidation warns

- **WHEN** the OpenFGA write for an `update_access` message succeeds
- **AND** cache invalidation subsequently fails
- **THEN** the service ACKs the message
- **AND** the message is not redelivered

#### Scenario: OpenFGA write fails before positive cache seeding

- **WHEN** an access mutation contains a new direct grant
- **AND** the corresponding OpenFGA write fails
- **THEN** the service does not seed a positive cache entry for that grant
- **AND** the message remains unacknowledged for redelivery

#### Scenario: ACK or TERM publication fails

- **WHEN** `Ack()` or `Term()` returns an error
- **THEN** its success counter is not incremented
- **AND** the acknowledgement failure is logged and traced
- **AND** the message remains unacknowledged for server-managed redelivery

#### Scenario: OpenFGA validation is not blanket-terminal

- **WHEN** OpenFGA reports a recognized invalid tuple
- **THEN** `FgaService` removes that tuple and continues the remaining batch
- **AND** the delivery outcome follows the final batch result

#### Scenario: Propagated OpenFGA validation remains transient

- **WHEN** another OpenFGA validation error propagates from `FgaService`
- **THEN** the delivery wrapper leaves the message unacknowledged as transient

### Requirement: Updates and deletions are applied in global stream order

The consumer SHALL enforce one globally pending update or delete access message
through consumer-wide `MaxAckPending: 1`. A transiently failed unacknowledged
message SHALL occupy that pending slot until it succeeds, terminates, or reaches
`MaxDeliver`. A newer stream sequence SHALL NOT be processed while an older
sequence is pending redelivery. The final `30m` BackOff value SHALL repeat once
after the seventh delivery before the max-delivery advisory, making
authoritative exhaustion approximately 94 minutes after initial delivery.
Later messages SHALL remain durably stored during that accepted window.
`delete_access` SHALL remove publisher-managed tuples while preserving the
existing externally managed `team:*` grants.

#### Scenario: Newer update waits behind transient failure

- **WHEN** stream sequence N fails transiently
- **AND** sequence N+1 is already stored
- **THEN** sequence N+1 remains pending while N follows its BackOff schedule
- **AND** N+1 is processed only after N succeeds, terminates, or exhausts

#### Scenario: Deletion cannot be bypassed by an older update

- **WHEN** an `update_access` message fails transiently
- **AND** a later `delete_access` for the same object is stored
- **THEN** the deletion remains pending behind the older update
- **AND** after recovery the update is applied before the deletion
- **AND** the older update cannot recreate tuples after deletion

#### Scenario: Multiple replicas share the ordering limit

- **WHEN** multiple fga-sync replicas bind the same durable consumer
- **THEN** `MaxAckPending: 1` applies across those replicas
- **AND** only one update or delete access message is outstanding globally

### Requirement: Every processing attempt has a hard deadline

The wrapper SHALL create a 90-second context deadline for every handler attempt.
The deadline SHALL be enforced by OpenFGA reads and writes and SHALL expire
before the first two-minute consumer BackOff timeout.

#### Scenario: OpenFGA call blocks

- **WHEN** an OpenFGA operation has not completed within 90 seconds
- **THEN** the processing context is canceled
- **AND** the attempt is classified as transient
- **AND** the message remains unacknowledged for JetStream redelivery

### Requirement: Final-failure recovery uses authoritative current state

Terminal outcomes and max-delivery advisories SHALL record the stream sequence,
classification, delivery count, object type, and safe resource context.
Recovery SHALL NOT blindly republish the retained message. Operators SHALL use
the evidence to have the owning service re-read its current database state and
publish a fresh update or deletion as appropriate.

#### Scenario: Exhausted payload is older than later state

- **WHEN** an exhausted message requires recovery
- **AND** the owning resource may have changed since the retained message was
  first published
- **THEN** the owning service derives a fresh full-state payload from its current
  database record
- **AND** only that fresh payload is published for recovery

### Requirement: Cutover and rollback prevent mixed delivery paths

Production cutover and rollback SHALL use a one-time operational platform
maintenance window across every publisher of either migrated subject. This
change SHALL NOT require new publisher maintenance-mode code. The runbook SHALL
define the platform controls used to pause mutation ingress and asynchronous
producers, and operations SHALL verify publication has stopped. The stream
SHALL be provisioned without an active consumer, both old core subscribers
SHALL drain and stop, the captured core-processed duplicate backlog SHALL be
purged, and operations SHALL verify the stream is empty. The service SHALL then
create a `DeliverNewPolicy` durable consumer against the empty stream before
the window is released.

#### Scenario: Phase 1 production cutover

- **WHEN** LFXV2-2830 and LFXV2-2828 are deployed and verified
- **AND** the operational maintenance window is enabled
- **THEN** both old core subscribers are drained and stopped
- **AND** the core-processed duplicate backlog is purged
- **AND** the stream is verified empty
- **AND** the JetStream-enabled binary is deployed only after old core
  subscribers stop
- **AND** its `DeliverNewPolicy` durable consumer starts against the empty
  stream
- **AND** the maintenance window is released only after no core subscription
  remains

#### Scenario: Rollback to core consumption

- **WHEN** the JetStream consumer must be rolled back
- **THEN** the operational maintenance window is enabled
- **AND** publication remains paused until every durable message has an explicit
  order-safe disposition
- **AND** the preferred path drains and ACKs the stream backlog in sequence
  order before stopping the consumer and restoring core subscriptions
- **AND** if the consumer cannot safely drain, operations record every affected
  object, restore core subscriptions without overlap, trigger fresh
  authoritative-state publication, verify convergence, and purge the superseded
  retained messages
- **AND** the window is not released with undispositioned durable messages
- **AND** retained payloads are not blindly bulk-replayed

### Requirement: Resilient NATS reconnection

The service SHALL attempt to reconnect to NATS indefinitely and SHALL NOT exit
the process because reconnect attempts were exhausted, so that a transient NATS
outage does not terminate the service. Every NATS `ClosedHandler` path SHALL
release `gracefulCloseWG` exactly once.

On intentional shutdown the service SHALL call `ConsumeContext.Stop()`, then
wait for `ConsumeContext.Closed()` up to a bounded grace period before
canceling the service context, and then drain NATS. It SHALL NOT cancel the
service context before `Closed()` fires or the grace period elapses, because
`Closed()` only fires once any in-flight delivery attempt has returned, and
canceling first would abort an attempt that could otherwise succeed. It SHALL
NOT call both `Stop()` and `Drain()` on the consume context. `/readyz` SHALL
continue to report NATS connection and draining state; this does not attest
that the JetStream consume loop is healthy, and consumer-aware readiness is
outside Phase 1.

#### Scenario: NATS connection drops and recovers

- **WHEN** the NATS connection is lost
- **THEN** the service keeps attempting to reconnect without exiting
- **AND** on reconnection it resumes consuming update and delete access messages

#### Scenario: Intentional shutdown still works

- **WHEN** the service receives SIGINT or SIGTERM
- **THEN** it calls `Stop()` on the JetStream consume context
- **AND** it waits for `Closed()` within a bounded grace period, canceling the
  service context only if that period elapses
- **AND** it drains the NATS connection afterward
- **AND** unacknowledged work remains available for redelivery

### Requirement: Delivery observability

The service SHALL expose `sync_ack`, `sync_transient_attempts`,
`sync_terminal`, and `sync_max_deliver_exhausted` counters at `/debug/vars`.
`sync_transient_attempts` SHALL increment once whenever the wrapper leaves a
failed delivery unacknowledged, so one exhausted message can increment it seven
times. `sync_max_deliver_exhausted` SHALL increment once from a server advisory
received while a fga-sync replica is connected. The service SHALL consume
JetStream max-delivery advisories through a core NATS queue subscription.
External platform monitoring SHALL subscribe and alert on the same advisory
subject so expiry after the last replica disconnects is not silent. A durable
in-service advisory stream and a separate termination-advisory subscription are
not required in Phase 1. `sync_terminal` SHALL increment and log the locally
known outcome when the wrapper sends TERM. Every processing attempt SHALL
preserve incoming trace context and emit the existing `nats.process` consumer
span. Additional JetStream-specific span attributes are deferred.

For each max-delivery advisory received by the service, it SHALL fetch the
retained message by stream sequence using `Stream.GetMsg` and decode safe object
context. If message lookup or decoding fails, the service SHALL still increment
`sync_max_deliver_exhausted` and log the advisory identity, sequence, delivery
count, and enrichment error.

#### Scenario: Counters reflect processing outcomes

- **WHEN** update and delete access messages are processed with a mix of success,
  transient failure, and terminal failure
- **THEN** the corresponding `/debug/vars` counters increase accordingly

#### Scenario: Connected service reports exhaustion

- **WHEN** a connected fga-sync replica receives a max-delivery advisory
- **THEN** `sync_max_deliver_exhausted` increases
- **AND** the log contains the stream sequence and safe recovery context

#### Scenario: External monitoring covers disconnected service

- **WHEN** JetStream emits a max-delivery advisory while no fga-sync replica is
  connected
- **THEN** external platform monitoring records and alerts on the advisory
- **AND** operators use its stream sequence for fresh-state recovery

### Requirement: Platform verification proves outage recovery

The external platform integration stack SHALL verify the durable behavior with
real NATS JetStream and OpenFGA dependencies before production rollout
approval. The drill SHALL NOT execute inside the live maintenance window or its
gate-release sequence.

#### Scenario: OpenFGA outage with queued update and deletion

- **WHEN** OpenFGA is stopped
- **AND** a valid `update_access` is followed by `delete_access` for the same
  object
- **THEN** the first message is redelivered according to BackOff
- **AND** the deletion remains pending behind it
- **AND** after OpenFGA is restored before exhaustion, the update then deletion
  are applied in stream order and acknowledged
- **AND** no publisher-managed tuples remain for the deleted object
- **AND** pre-existing externally managed `team:*` grants remain unchanged

#### Scenario: Messages published while fga-sync is offline

- **GIVEN** the durable consumer already exists
- **WHEN** both migrated subjects are published while no fga-sync instance runs
- **THEN** the stream retains the messages
- **AND** a later instance receives and processes them in stream order

#### Scenario: External verification of durable-state-loss recovery

- **GIVEN** a disposable non-production durable has processed test history
- **WHEN** that durable is deleted and recreated through normal service startup
- **THEN** retained history predating recreation is not redelivered
- **AND** a message published after recreation is delivered and acknowledged
- **AND** access-check and core-subscription handlers remain available
- **AND** no stream purge or manual consumer creation is required
