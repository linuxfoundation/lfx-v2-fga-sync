// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	nats "github.com/nats-io/nats.go"
	"github.com/openfga/go-sdk/client"
)

// HandlerService is the service that handles the messages from NATS about FGA syncing.
type HandlerService struct {
	fgaService FgaService
}

// buildObjectID constructs a standardized object identifier from type and UID.
// This ensures consistent object identifier construction across all handlers.
// Format: "objectType:uid" (e.g., "committee:123", "project:abc-def")
func buildObjectID(objectType, uid string) string {
	return fmt.Sprintf("%s:%s", objectType, uid)
}

// standardAccessStub represents the default structure for access control objects
type standardAccessStub struct {
	UID        string              `json:"uid"`
	ObjectType string              `json:"object_type"`
	Public     bool                `json:"public"`
	Relations  map[string][]string `json:"relations"`
	References map[string][]string `json:"references"`
}

// INatsMsg is an interface for [nats.Msg] that allows for mocking.
type INatsMsg interface {
	Reply() string
	Respond(data []byte) error
	Data() []byte
	Subject() string
	Header() nats.Header
}

// NatsMsg is a wrapper around [nats.Msg] that implements [INatsMsg].
type NatsMsg struct {
	*nats.Msg
}

// Reply implements [INatsMsg.Reply].
func (m *NatsMsg) Reply() string {
	return m.Msg.Reply
}

// Respond implements [INatsMsg.Respond].
func (m *NatsMsg) Respond(data []byte) error {
	return m.Msg.Respond(data)
}

// Data implements [INatsMsg.Data].
func (m *NatsMsg) Data() []byte {
	return m.Msg.Data
}

// Subject implements [INatsMsg.Subject].
func (m *NatsMsg) Subject() string {
	return m.Msg.Subject
}

// Header implements [INatsMsg.Header].
func (m *NatsMsg) Header() nats.Header {
	return m.Msg.Header
}

// jetStreamMessage is the subset of [jetstream.Msg] needed to adapt a JetStream
// message to [INatsMsg]. It is satisfied by [jetstream.Msg] and by test doubles.
type jetStreamMessage interface {
	Data() []byte
	Headers() nats.Header
	Subject() string
}

// jetStreamNatsMsg adapts a JetStream message to [INatsMsg]. Unlike [NatsMsg], it
// hides the ACK reply subject: JetStream acknowledgment is handled explicitly by
// the access mutation consumer, not via a reply-subject publish, so Reply always
// returns empty and Respond is a no-op.
type jetStreamNatsMsg struct {
	message jetStreamMessage
}

var _ INatsMsg = (*jetStreamNatsMsg)(nil)

// newJetStreamNatsMsg wraps a JetStream message as an [INatsMsg].
func newJetStreamNatsMsg(message jetStreamMessage) INatsMsg {
	return &jetStreamNatsMsg{message: message}
}

// Data implements [INatsMsg.Data].
func (m *jetStreamNatsMsg) Data() []byte {
	return m.message.Data()
}

// Header implements [INatsMsg.Header].
func (m *jetStreamNatsMsg) Header() nats.Header {
	return m.message.Headers()
}

// Reply implements [INatsMsg.Reply]. JetStream access mutations send no reply.
func (m *jetStreamNatsMsg) Reply() string {
	return ""
}

// Respond implements [INatsMsg.Respond] as a no-op; acknowledgment happens via
// Ack/Term on the underlying JetStream message, not a reply-subject publish.
func (m *jetStreamNatsMsg) Respond(_ []byte) error {
	return nil
}

// Subject implements [INatsMsg.Subject].
func (m *jetStreamNatsMsg) Subject() string {
	return m.message.Subject()
}

// processStandardAccessUpdate handles the default access control update logic
func (h *HandlerService) processStandardAccessUpdate(
	ctx context.Context,
	message INatsMsg,
	obj *standardAccessStub,
	excludeRelations ...string,
) error {

	logger.With("object_type", obj.ObjectType, "uid", obj.UID).InfoContext(
		ctx,
		fmt.Sprintf("handling %s access control update", obj.ObjectType),
	)

	if obj.UID == "" {
		logger.ErrorContext(ctx, fmt.Sprintf("%s ID not found", obj.ObjectType))
		return newTerminalValidationError(fmt.Errorf("%s ID not found", obj.ObjectType))
	}

	object := buildObjectID(obj.ObjectType, obj.UID)

	// Build a list of tuples to sync.
	tuples := make([]client.ClientTupleKey, 0, 4)

	// Convert the "public" attribute to a "user:*" relation.
	if obj.Public {
		tuples = append(tuples, client.ClientTupleKey{
			User: constants.UserWildcard, Relation: constants.RelationViewer, Object: object,
		})
	}

	// for parent relation, project relation, etc
	for reference, valueList := range obj.References {
		refType := reference
		// When the reference is parent, use the object type itself as the reference type.
		// i.e. if the object type is committee, the parent relation should be committee:<parent_id>.
		if reference == constants.RelationParent {
			refType = obj.ObjectType
		}
		for _, value := range valueList {
			// Check if value already contains a type prefix (e.g., "committee:123")
			var key string
			if strings.Contains(value, ":") {
				// Validate type:id format - must have exactly one colon with non-empty parts
				parts := strings.SplitN(value, ":", 2)
				if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
					logger.ErrorContext(ctx, "invalid reference format: must be 'type:id' with both parts non-empty",
						"reference", reference,
					)
					return newTerminalValidationError(
						errors.New("invalid reference format: must be 'type:id' with both parts non-empty"),
					)
				}
				// Value already has valid type:id format, use as-is
				key = value
			} else {
				// Value is just an ID, prepend the type
				key = fmt.Sprintf("%s:%s", refType, value)
			}
			tuples = append(tuples, client.ClientTupleKey{User: key, Relation: reference, Object: object})
		}
	}

	// Add each principal from the object as the corresponding relationship tuple
	// (as defined in the OpenFGA schema).
	// for writer, auditor etc
	for relation, principals := range obj.Relations {
		for _, principal := range principals {
			tuples = append(tuples, client.ClientTupleKey{
				User: constants.ObjectTypeUser + principal, Relation: relation, Object: object,
			})
		}
	}

	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, tuples, excludeRelations...)
	if err != nil {
		logger.With(
			"error_type", safeErrorType(err),
			"tuples", tuples,
			"object", object,
		).ErrorContext(ctx, "failed to sync tuples")
		return err
	}

	logger.With(
		"tuples", tuples,
		"object", object,
		"writes", tuplesWrites,
		"deletes", tuplesDeletes,
	).InfoContext(ctx, "synced tuples")

	if message.Reply() != "" {
		// Send a reply if an inbox was provided.
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, fmt.Sprintf("sent %s access control update response", obj.ObjectType))
	}

	return nil
}
