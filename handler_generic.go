// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/openfga/go-sdk/client"
)

// GenericFGAMessage is the universal message format for all FGA operations.
// This allows clients to send resource-agnostic messages without needing
// to know about resource-specific NATS subjects or message formats.
type GenericFGAMessage struct {
	ObjectType string                 `json:"object_type"` // e.g., "committee", "project", "meeting"
	Operation  string                 `json:"operation"`   // e.g., "update_access", "member_put"
	Data       map[string]interface{} `json:"data"`        // Operation-specific payload
}

// UnmarshalData unmarshals the data field into a specific type
func (m *GenericFGAMessage) UnmarshalData(v interface{}) error {
	bytes, err := json.Marshal(m.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, v)
}

// GenericAccessData represents the data field for update_access operations
type GenericAccessData struct {
	UID              string              `json:"uid"`
	Public           bool                `json:"public"`
	Relations        map[string][]string `json:"relations"`         // relation_name → [usernames]
	References       map[string][]string `json:"references"`        // relation_name → [object_uids]
	ExcludeRelations []string            `json:"exclude_relations"` // Optional: relations managed elsewhere
}

// GenericDeleteData represents the data field for delete_access operations
type GenericDeleteData struct {
	UID string `json:"uid"`
}

// GenericMemberData represents the data field for member_put/member_remove operations.
// Supports multiple relations for a single user, enabling atomic updates.
type GenericMemberData struct {
	UID                   string   `json:"uid"`
	Username              string   `json:"username"`
	Relations             []string `json:"relations"`               // Multiple relations supported
	MutuallyExclusiveWith []string `json:"mutually_exclusive_with"` // Optional: auto-remove these
}

// genericUpdateAccessHandler handles universal update_access operations.
// This provides a resource-agnostic way for clients to update access control
// without needing resource-specific handlers.
//
// NATS Subject: lfx.fga-sync.update_access
//
// Message Format:
//
//	{
//	  "object_type": "committee",
//	  "operation": "update_access",
//	  "data": {
//	    "uid": "committee-123",
//	    "public": true,
//	    "relations": {"member": ["user1", "user2"]},
//	    "references": {"project": ["project-456"]},
//	    "exclude_relations": ["participant"]
//	  }
//	}
func (h *HandlerService) genericUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse generic message
	genericMsg := new(GenericFGAMessage)
	if err := json.Unmarshal(message.Data(), genericMsg); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to parse generic message")
		return err
	}

	// Validate
	if genericMsg.ObjectType == "" {
		logger.ErrorContext(ctx, "object_type is required")
		return errors.New("object_type is required")
	}
	if genericMsg.Operation != "update_access" {
		logger.ErrorContext(ctx, "invalid operation for this handler", "operation", genericMsg.Operation)
		return errors.New("invalid operation for update_access handler")
	}

	// Parse data field
	data := new(GenericAccessData)
	if err := genericMsg.UnmarshalData(data); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to parse access data")
		return err
	}

	logger.With(
		"object_type", genericMsg.ObjectType,
		"uid", data.UID,
	).InfoContext(ctx, "handling generic update_access")

	// Convert to standardAccessStub (reuse existing generic logic)
	stub := &standardAccessStub{
		UID:        data.UID,
		ObjectType: genericMsg.ObjectType,
		Public:     data.Public,
		Relations:  data.Relations,
		References: data.References,
	}

	// Use existing generic handler
	return h.processStandardAccessUpdate(message, stub, data.ExcludeRelations...)
}

// genericDeleteAccessHandler handles universal delete_access operations.
// This removes all tuples for a resource (typically used when a resource is deleted).
//
// NATS Subject: lfx.fga-sync.delete_access
//
// Message Format:
//
//	{
//	  "object_type": "committee",
//	  "operation": "delete_access",
//	  "data": {
//	    "uid": "committee-123"
//	  }
//	}
func (h *HandlerService) genericDeleteAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse generic message
	genericMsg := new(GenericFGAMessage)
	if err := json.Unmarshal(message.Data(), genericMsg); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to parse generic message")
		return err
	}

	// Validate
	if genericMsg.ObjectType == "" {
		logger.ErrorContext(ctx, "object_type is required")
		return errors.New("object_type is required")
	}
	if genericMsg.Operation != "delete_access" {
		logger.ErrorContext(ctx, "invalid operation for this handler", "operation", genericMsg.Operation)
		return errors.New("invalid operation for delete_access handler")
	}

	// Parse data field
	data := new(GenericDeleteData)
	if err := genericMsg.UnmarshalData(data); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to parse delete data")
		return err
	}

	// Validate UID is non-empty
	if data.UID == "" {
		logger.ErrorContext(ctx, "uid is required")
		return errors.New("uid is required")
	}

	logger.With(
		"object_type", genericMsg.ObjectType,
		"uid", data.UID,
	).InfoContext(ctx, "handling generic delete_access")

	// Build object identifier using standard helper
	object := buildObjectID(genericMsg.ObjectType, data.UID)

	// Use existing generic sync with empty tuples (deletes all)
	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, nil)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to delete access")
		return err
	}

	logger.With(
		"object", object,
		"writes", tuplesWrites,
		"deletes", tuplesDeletes,
	).InfoContext(ctx, "deleted all access for "+genericMsg.ObjectType)

	// Send reply
	if message.Reply() != "" {
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}
	}

	return nil
}

// genericMemberPutHandler handles universal member_put operations with support for multiple relations.
// This allows adding a user to a resource with one or more relations atomically.
//
// NATS Subject: lfx.fga-sync.member_put
//
// Message Format (single relation):
//
//	{
//	  "object_type": "committee",
//	  "operation": "member_put",
//	  "data": {
//	    "uid": "committee-123",
//	    "username": "user-alice",
//	    "relations": ["member"]
//	  }
//	}
//
// Message Format (multiple relations):
//
//	{
//	  "object_type": "past_meeting",
//	  "operation": "member_put",
//	  "data": {
//	    "uid": "past-meeting-123",
//	    "username": "user-alice",
//	    "relations": ["host", "invitee"]
//	  }
//	}
//
// Message Format (mutually exclusive):
//
//	{
//	  "object_type": "meeting",
//	  "operation": "member_put",
//	  "data": {
//	    "uid": "meeting-123",
//	    "username": "user-alice",
//	    "relations": ["host"],
//	    "mutually_exclusive_with": ["participant", "host"]
//	  }
//	}
func (h *HandlerService) genericMemberPutHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse and validate message
	genericMsg, data, err := h.parseAndValidateMemberPutMessage(ctx, message)
	if err != nil {
		return err
	}

	logger.With(
		"object_type", genericMsg.ObjectType,
		"uid", data.UID,
		"username", data.Username,
		"relations", data.Relations,
	).InfoContext(ctx, "handling generic member_put")

	// Build identifiers using standard helper
	object := buildObjectID(genericMsg.ObjectType, data.UID)
	userPrincipal := constants.ObjectTypeUser + data.Username

	// Compute tuple changes
	tuplesToWrite, tuplesToDelete, err := h.computeMemberPutChanges(ctx, object, userPrincipal, data)
	if err != nil {
		return err
	}

	// Apply changes
	err = h.applyMemberPutChanges(
		ctx, genericMsg.ObjectType, object, userPrincipal, data.Relations, tuplesToWrite, tuplesToDelete,
	)
	if err != nil {
		return err
	}

	// Send reply
	return h.sendReplyIfNeeded(ctx, message)
}

// parseAndValidateMemberPutMessage parses and validates the member_put message
func (h *HandlerService) parseAndValidateMemberPutMessage(
	ctx context.Context, message INatsMsg,
) (*GenericFGAMessage, *GenericMemberData, error) {
	// Parse generic message
	genericMsg := new(GenericFGAMessage)
	if err := json.Unmarshal(message.Data(), genericMsg); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to parse generic message")
		return nil, nil, err
	}

	// Validate object_type
	if genericMsg.ObjectType == "" {
		logger.ErrorContext(ctx, "object_type is required")
		return nil, nil, errors.New("object_type is required")
	}
	if genericMsg.Operation != "member_put" {
		logger.ErrorContext(ctx, "invalid operation for this handler", "operation", genericMsg.Operation)
		return nil, nil, errors.New("invalid operation for member_put handler")
	}

	// Parse data field
	data := new(GenericMemberData)
	if err := genericMsg.UnmarshalData(data); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to parse member data")
		return nil, nil, err
	}

	// Validate required fields
	if data.Username == "" {
		logger.ErrorContext(ctx, "username is required")
		return nil, nil, errors.New("username is required")
	}
	if data.UID == "" {
		logger.ErrorContext(ctx, "uid is required")
		return nil, nil, errors.New("uid is required")
	}
	if len(data.Relations) == 0 {
		logger.ErrorContext(ctx, "relations array cannot be empty")
		return nil, nil, errors.New("relations array cannot be empty")
	}
	// Validate each relation is non-empty
	for _, relation := range data.Relations {
		if relation == "" {
			logger.ErrorContext(ctx, "relation value cannot be empty")
			return nil, nil, errors.New("relation value cannot be empty")
		}
	}

	return genericMsg, data, nil
}

// computeMemberPutChanges determines which tuples to write and delete
func (h *HandlerService) computeMemberPutChanges(
	ctx context.Context,
	object, userPrincipal string,
	data *GenericMemberData,
) ([]client.ClientTupleKey, []client.ClientTupleKeyWithoutCondition, error) {
	// Build mutually exclusive map for quick lookup
	mutuallyExclusiveMap := make(map[string]bool)
	for _, rel := range data.MutuallyExclusiveWith {
		mutuallyExclusiveMap[rel] = true
	}

	// Read existing tuples
	existingTuples, err := h.fgaService.ReadObjectTuples(ctx, object)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read existing tuples",
			errKey, err,
			"user", userPrincipal,
			"object", object,
		)
		return nil, nil, err
	}

	// Build desired relations set
	desiredRelations := make(map[string]bool)
	for _, rel := range data.Relations {
		desiredRelations[rel] = true
	}

	// Determine what to write and delete
	var tuplesToWrite []client.ClientTupleKey
	var tuplesToDelete []client.ClientTupleKeyWithoutCondition
	existingRelationsMap := make(map[string]bool)

	for _, tuple := range existingTuples {
		if tuple.Key.User == userPrincipal {
			existingRelationsMap[tuple.Key.Relation] = true

			// If this relation is mutually exclusive and NOT desired, delete it
			if mutuallyExclusiveMap[tuple.Key.Relation] && !desiredRelations[tuple.Key.Relation] {
				tuplesToDelete = append(tuplesToDelete, client.ClientTupleKeyWithoutCondition{
					User:     tuple.Key.User,
					Relation: tuple.Key.Relation,
					Object:   tuple.Key.Object,
				})
			}
		}
	}

	// Add relations that don't exist yet
	for relation := range desiredRelations {
		if !existingRelationsMap[relation] {
			tuplesToWrite = append(tuplesToWrite, h.fgaService.TupleKey(userPrincipal, relation, object))
		}
	}

	return tuplesToWrite, tuplesToDelete, nil
}

// applyMemberPutChanges applies the computed tuple changes
func (h *HandlerService) applyMemberPutChanges(
	ctx context.Context,
	objectType, object, userPrincipal string,
	relations []string,
	tuplesToWrite []client.ClientTupleKey,
	tuplesToDelete []client.ClientTupleKeyWithoutCondition,
) error {
	if len(tuplesToWrite) > 0 || len(tuplesToDelete) > 0 {
		err := h.fgaService.WriteAndDeleteTuples(ctx, tuplesToWrite, tuplesToDelete)
		if err != nil {
			logger.ErrorContext(ctx, "failed to put member relations",
				errKey, err,
				"user", userPrincipal,
				"relations", relations,
				"object", object,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"relations", relations,
			"object", object,
			"writes", len(tuplesToWrite),
			"deletes", len(tuplesToDelete),
		).InfoContext(ctx, "put member to "+objectType)
	} else {
		logger.With(
			"user", userPrincipal,
			"relations", relations,
			"object", object,
		).InfoContext(ctx, "member already has correct relations - no changes needed")
	}

	return nil
}

// sendReplyIfNeeded sends a reply message if requested
func (h *HandlerService) sendReplyIfNeeded(ctx context.Context, message INatsMsg) error {
	if message.Reply() != "" {
		if err := message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}
	}
	return nil
}

// genericMemberRemoveHandler handles universal member_remove operations with support for multiple relations.
// If relations array is empty, removes ALL relations for the user.
// If relations array is provided, removes only those specific relations.
//
// NATS Subject: lfx.fga-sync.member_remove
//
// Message Format (remove specific relations):
//
//	{
//	  "object_type": "past_meeting",
//	  "operation": "member_remove",
//	  "data": {
//	    "uid": "past-meeting-123",
//	    "username": "user-alice",
//	    "relations": ["host", "invitee"]
//	  }
//	}
//
// Message Format (remove all relations):
//
//	{
//	  "object_type": "committee",
//	  "operation": "member_remove",
//	  "data": {
//	    "uid": "committee-123",
//	    "username": "user-alice",
//	    "relations": []
//	  }
//	}
func (h *HandlerService) genericMemberRemoveHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse generic message
	genericMsg := new(GenericFGAMessage)
	if err := json.Unmarshal(message.Data(), genericMsg); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to parse generic message")
		return err
	}

	// Validate
	if genericMsg.ObjectType == "" {
		logger.ErrorContext(ctx, "object_type is required")
		return errors.New("object_type is required")
	}
	if genericMsg.Operation != "member_remove" {
		logger.ErrorContext(ctx, "invalid operation for this handler", "operation", genericMsg.Operation)
		return errors.New("invalid operation for member_remove handler")
	}

	// Parse data field
	data := new(GenericMemberData)
	if err := genericMsg.UnmarshalData(data); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to parse member data")
		return err
	}

	// Validate required fields
	if data.Username == "" {
		logger.ErrorContext(ctx, "username is required")
		return errors.New("username is required")
	}
	if data.UID == "" {
		logger.ErrorContext(ctx, "uid is required")
		return errors.New("uid is required")
	}

	logger.With(
		"object_type", genericMsg.ObjectType,
		"uid", data.UID,
		"username", data.Username,
		"relations", data.Relations,
	).InfoContext(ctx, "handling generic member_remove")

	// Build identifiers using standard helper
	object := buildObjectID(genericMsg.ObjectType, data.UID)
	userPrincipal := constants.ObjectTypeUser + data.Username

	// Filter out empty relations and build list of valid relations to delete
	var validRelations []string
	for _, relation := range data.Relations {
		if relation != "" {
			validRelations = append(validRelations, relation)
		}
	}

	// If no specific relations provided (or all were empty), delete ALL relations for this user
	if len(validRelations) == 0 {
		err := h.fgaService.DeleteTuplesByUserAndObject(ctx, userPrincipal, object)
		if err != nil {
			logger.ErrorContext(ctx, "failed to remove all member relations",
				errKey, err,
				"user", userPrincipal,
				"object", object,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"object", object,
		).InfoContext(ctx, "removed all relations from "+genericMsg.ObjectType)
	} else {
		// Delete specific relations (only non-empty ones)
		var tuplesToDelete []client.ClientTupleKeyWithoutCondition
		for _, relation := range validRelations {
			tuplesToDelete = append(tuplesToDelete, client.ClientTupleKeyWithoutCondition{
				User:     userPrincipal,
				Relation: relation,
				Object:   object,
			})
		}

		// Use WriteAndDeleteTuples with empty writes
		err := h.fgaService.WriteAndDeleteTuples(ctx, nil, tuplesToDelete)
		if err != nil {
			logger.ErrorContext(ctx, "failed to remove member relations",
				errKey, err,
				"user", userPrincipal,
				"relations", validRelations,
				"object", object,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"relations", validRelations,
			"object", object,
			"deletes", len(tuplesToDelete),
		).InfoContext(ctx, "removed member from "+genericMsg.ObjectType)
	}

	// Send reply
	if message.Reply() != "" {
		if err := message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}
	}

	return nil
}
