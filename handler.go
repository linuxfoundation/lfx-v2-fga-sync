// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	nats "github.com/nats-io/nats.go"
	"github.com/openfga/go-sdk/client"
)

// HandlerService is the service that handles the messages from NATS about FGA syncing.
type HandlerService struct {
	fgaService FgaService
}

// standardAccessStub represents the default structure for access control objects
type standardAccessStub struct {
	UID        string              `json:"uid"`
	ObjectType string              `json:"object_type"`
	Public     bool                `json:"public"`
	Relations  map[string][]string `json:"relations"`
	References map[string][]string `json:"references"`
}

// memberOperationStub represents a generic member operation message
type memberOperationStub struct {
	Username  string `json:"username"`
	ObjectUID string `json:"object_uid"` // committee_uid, mailing_list_uid, etc.
}

// memberOperationConfig configures the behavior of member operations
type memberOperationConfig struct {
	objectTypePrefix       string   // e.g., "committee:"
	objectTypeName         string   // e.g., "committee" (for logging)
	relation               string   // e.g., constants.RelationMember
	mutuallyExclusiveWith  []string // Optional: relations that should be removed when this relation is added
}

// memberOperation defines the type of operation to perform on a member
type memberOperation int

const (
	memberOperationPut memberOperation = iota
	memberOperationRemove
)

// INatsMsg is an interface for [nats.Msg] that allows for mocking.
type INatsMsg interface {
	Reply() string
	Respond(data []byte) error
	Data() []byte
	Subject() string
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

// processStandardAccessUpdate handles the default access control update logic
func (h *HandlerService) processStandardAccessUpdate(message INatsMsg, obj *standardAccessStub, excludeRelations ...string) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling "+obj.ObjectType+" access control update")

	if obj.UID == "" {
		logger.ErrorContext(ctx, obj.ObjectType+" ID not found")
		return errors.New(obj.ObjectType + " ID not found")
	}

	object := fmt.Sprintf("%s:%s", obj.ObjectType, obj.UID)

	// Build a list of tuples to sync.
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Convert the "public" attribute to a "user:*" relation.
	if obj.Public {
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))
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
			key := fmt.Sprintf("%s:%s", refType, value)
			tuples = append(tuples, h.fgaService.TupleKey(key, reference, object))
		}
	}

	// Add each principal from the object as the corresponding relationship tuple
	// (as defined in the OpenFGA schema).
	// for writer, auditor etc
	for relation, principals := range obj.Relations {
		for _, principal := range principals {
			tuples = append(tuples, h.fgaService.TupleKey(constants.ObjectTypeUser+principal, relation, object))
		}
	}

	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, tuples, excludeRelations...)
	if err != nil {
		logger.With(errKey, err, "tuples", tuples, "object", object).ErrorContext(ctx, "failed to sync tuples")
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

		logger.With("object", object).InfoContext(ctx, "sent "+obj.ObjectType+" access control update response")
	}

	return nil
}

// processDeleteAllAccessMessage handles the common logic for deleting all access tuples for an object
func (h *HandlerService) processDeleteAllAccessMessage(
	message INatsMsg,
	objectTypePrefix,
	objectTypeName string,
) error {
	ctx := context.Background()

	logger.InfoContext(
		ctx,
		"handling "+objectTypeName+" access control delete all",
		"message", string(message.Data()),
	)

	objectUID := string(message.Data())
	if objectUID == "" {
		logger.ErrorContext(ctx, "empty deletion payload")
		return errors.New("empty deletion payload")
	}
	if objectUID[0] == '{' || objectUID[0] == '[' || objectUID[0] == '"' {
		// This event payload is not supposed to be serialized.
		logger.ErrorContext(ctx, "unsupported deletion payload")
		return errors.New("unsupported deletion payload")
	}

	object := objectTypePrefix + objectUID

	// Since this is a delete, we can call SyncObjectTuples directly
	// with a zero-value (nil) slice.
	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, nil)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to sync tuples")
		return err
	}

	logger.InfoContext(
		ctx,
		"synced tuples",
		"object", object,
		"writes", tuplesWrites,
		"deletes", tuplesDeletes,
	)

	if message.Reply() != "" {
		// Send a reply if an inbox was provided.
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, "sent "+objectTypeName+" access control delete all response")
	}

	return nil
}

// processMemberOperation handles member put/remove operations generically
func (h *HandlerService) processMemberOperation(
	message INatsMsg,
	member *memberOperationStub,
	operation memberOperation,
	config memberOperationConfig,
) error {
	ctx := context.Background()

	// Log the operation type
	operationType := "put"
	if operation == memberOperationRemove {
		operationType = "remove"
	}

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling "+config.objectTypeName+" member "+operationType)

	// Validate
	if member.Username == "" {
		logger.ErrorContext(ctx, config.objectTypeName+" member username not found")
		return errors.New(config.objectTypeName + " member username not found")
	}
	if member.ObjectUID == "" {
		logger.ErrorContext(ctx, config.objectTypeName+" UID not found")
		return errors.New(config.objectTypeName + " UID not found")
	}

	// Build identifiers
	objectFull := config.objectTypePrefix + member.ObjectUID
	userPrincipal := constants.ObjectTypeUser + member.Username

	// Execute operation
	var err error
	if operation == memberOperationPut {
		err = h.putMember(ctx, userPrincipal, objectFull, config)
	} else {
		err = h.removeMember(ctx, userPrincipal, objectFull, config)
	}

	if err != nil {
		return err
	}

	// Send reply
	if message.Reply() != "" {
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.InfoContext(ctx, "sent "+config.objectTypeName+" member "+operationType+" response",
			"object", objectFull,
			"member", userPrincipal,
		)
	}

	return nil
}

// putMember implements idempotent put operation (generic)
func (h *HandlerService) putMember(
	ctx context.Context,
	userPrincipal, object string,
	config memberOperationConfig,
) error {
	// Read existing tuples
	existingTuples, err := h.fgaService.ReadObjectTuples(ctx, object)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read existing tuples",
			errKey, err,
			"user", userPrincipal,
			"object", object,
		)
		return err
	}

	// Build a map of mutually exclusive relations for quick lookup
	mutuallyExclusiveMap := make(map[string]bool)
	for _, rel := range config.mutuallyExclusiveWith {
		mutuallyExclusiveMap[rel] = true
	}

	// Check if relation already exists and find mutually exclusive relations to remove
	var hasRelation bool
	var tuplesToDelete []client.ClientTupleKeyWithoutCondition

	for _, tuple := range existingTuples {
		if tuple.Key.User == userPrincipal {
			if tuple.Key.Relation == config.relation {
				hasRelation = true
			} else if mutuallyExclusiveMap[tuple.Key.Relation] {
				// This is a mutually exclusive relation that needs to be removed
				tuplesToDelete = append(tuplesToDelete, client.ClientTupleKeyWithoutCondition{
					User:     tuple.Key.User,
					Relation: tuple.Key.Relation,
					Object:   tuple.Key.Object,
				})
			}
		}
	}

	// Prepare write operations
	var tuplesToWrite []client.ClientTupleKey
	if !hasRelation {
		tuplesToWrite = append(tuplesToWrite, h.fgaService.TupleKey(userPrincipal, config.relation, object))
	}

	// Apply changes if needed
	if len(tuplesToWrite) > 0 || len(tuplesToDelete) > 0 {
		if err := h.fgaService.WriteAndDeleteTuples(ctx, tuplesToWrite, tuplesToDelete); err != nil {
			logger.ErrorContext(ctx, "failed to put member tuple",
				errKey, err,
				"user", userPrincipal,
				"relation", config.relation,
				"object", object,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"relation", config.relation,
			"object", object,
		).InfoContext(ctx, "put member to "+config.objectTypeName)
	} else {
		logger.With(
			"user", userPrincipal,
			"relation", config.relation,
			"object", object,
		).InfoContext(ctx, "member already has correct relation - no changes needed")
	}

	return nil
}

// removeMember removes a member relation (generic)
func (h *HandlerService) removeMember(
	ctx context.Context,
	userPrincipal, object string,
	config memberOperationConfig,
) error {
	err := h.fgaService.DeleteTuple(ctx, userPrincipal, config.relation, object)
	if err != nil {
		logger.ErrorContext(ctx, "failed to remove member tuple",
			errKey, err,
			"user", userPrincipal,
			"relation", config.relation,
			"object", object,
		)
		return err
	}

	logger.With(
		"user", userPrincipal,
		"relation", config.relation,
		"object", object,
	).InfoContext(ctx, "removed member from "+config.objectTypeName)

	return nil
}
