// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	"github.com/openfga/go-sdk/client" // Only for client types, not the full SDK
)

// groupsioMailingListMemberStub represents the structure of GroupsIO mailing list member data for FGA sync.
type groupsioMailingListMemberStub struct {
	// UID is the mailing list member ID.
	UID string `json:"uid"`
	// Username is the username (i.e. LFID) of the member. This is the identity of the user object in FGA.
	Username string `json:"username"`
	// MailingListUID is the mailing list ID for the mailing list the member belongs to.
	MailingListUID string `json:"mailing_list_uid"`
}

// groupsioMailingListMemberOperation defines the type of operation to perform on a mailing list member
type groupsioMailingListMemberOperation int

const (
	groupsioMailingListMemberPut groupsioMailingListMemberOperation = iota
	groupsioMailingListMemberRemove
)

// processGroupsIOMailingListMemberMessage handles the complete message processing flow
// for mailing list member operations
func (h *HandlerService) processGroupsIOMailingListMemberMessage(
	message INatsMsg,
	operation groupsioMailingListMemberOperation,
) error {
	ctx := context.Background()

	// Log the operation type
	operationType := constants.OperationPut
	responseMsg := "sent groupsio mailing list member put response"
	if operation == groupsioMailingListMemberRemove {
		operationType = constants.OperationRemove
		responseMsg = "sent groupsio mailing list member remove response"
	}

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling groupsio mailing list member "+operationType)

	// Parse the event data.
	member := new(groupsioMailingListMemberStub)
	err := json.Unmarshal(message.Data(), member)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	funcLogger := logger.With("mailing_list_member", member)

	// Validate required fields.
	if member.Username == "" {
		funcLogger.ErrorContext(ctx, "groupsio mailing list member username not found")
		return errors.New("groupsio mailing list member username not found")
	}
	if member.MailingListUID == "" {
		funcLogger.ErrorContext(ctx, "groupsio mailing list UID not found")
		return errors.New("groupsio mailing list UID not found")
	}

	// Perform the FGA operation
	err = h.handleGroupsIOMailingListMemberOperation(ctx, member, operation)
	if err != nil {
		return err
	}

	// Send reply if requested
	if message.Reply() != "" {
		if err = message.Respond([]byte("OK")); err != nil {
			funcLogger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		funcLogger.InfoContext(ctx, responseMsg,
			"mailing_list", constants.ObjectTypeGroupsIOMailingList+member.MailingListUID,
			"member", constants.ObjectTypeUser+member.Username,
		)
	}

	return nil
}

// handleGroupsIOMailingListMemberOperation handles the FGA operation for putting/removing mailing list members
func (h *HandlerService) handleGroupsIOMailingListMemberOperation(
	ctx context.Context,
	member *groupsioMailingListMemberStub,
	operation groupsioMailingListMemberOperation,
) error {
	mailingListObject := constants.ObjectTypeGroupsIOMailingList + member.MailingListUID
	userPrincipal := constants.ObjectTypeUser + member.Username

	switch operation {
	case groupsioMailingListMemberPut:
		return h.putGroupsIOMailingListMember(ctx, userPrincipal, mailingListObject)
	case groupsioMailingListMemberRemove:
		return h.removeGroupsIOMailingListMember(ctx, userPrincipal, mailingListObject)
	default:
		return errors.New("unknown groupsio mailing list member operation")
	}
}

// putGroupsIOMailingListMember implements idempotent put operation for mailing list member relations
func (h *HandlerService) putGroupsIOMailingListMember(
	ctx context.Context,
	userPrincipal,
	mailingListObject string,
) error {
	// Read existing relations for this user on this mailing list
	existingTuples, err := h.fgaService.ReadObjectTuples(ctx, mailingListObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read existing groupsio mailing list tuples",
			errKey, err,
			"user", userPrincipal,
			"mailing_list", mailingListObject,
		)
		return err
	}

	// Check if the member relation already exists
	var hasMemberRelation bool
	for _, tuple := range existingTuples {
		if tuple.Key.User == userPrincipal && tuple.Key.Relation == constants.RelationMember {
			hasMemberRelation = true
			break
		}
	}

	// Only write if the relation doesn't already exist
	if !hasMemberRelation {
		tuples := []client.ClientTupleKey{
			h.fgaService.TupleKey(userPrincipal, constants.RelationMember, mailingListObject),
		}

		err = h.fgaService.WriteTuples(ctx, tuples)
		if err != nil {
			logger.ErrorContext(ctx, "failed to put groupsio mailing list member tuple",
				errKey, err,
				"user", userPrincipal,
				"relation", constants.RelationMember,
				"mailing_list", mailingListObject,
			)
			return err
		}

		logger.With(
			"user", userPrincipal,
			"relation", constants.RelationMember,
			"mailing_list", mailingListObject,
		).InfoContext(ctx, "put member to groupsio mailing list")
	} else {
		logger.With(
			"user", userPrincipal,
			"relation", constants.RelationMember,
			"mailing_list", mailingListObject,
		).InfoContext(ctx, "member already has correct relation - no changes needed")
	}

	return nil
}

// removeGroupsIOMailingListMember removes the member relation for a user from a mailing list
func (h *HandlerService) removeGroupsIOMailingListMember(
	ctx context.Context,
	userPrincipal,
	mailingListObject string,
) error {
	err := h.fgaService.DeleteTuple(ctx, userPrincipal, constants.RelationMember, mailingListObject)
	if err != nil {
		logger.ErrorContext(ctx, "failed to remove groupsio mailing list member tuple",
			errKey, err,
			"user", userPrincipal,
			"relation", constants.RelationMember,
			"mailing_list", mailingListObject,
		)
		return err
	}

	logger.With(
		"user", userPrincipal,
		"relation", constants.RelationMember,
		"mailing_list", mailingListObject,
	).InfoContext(ctx, "removed member from groupsio mailing list")

	return nil
}

// groupsioMailingListMemberPutHandler handles putting a member to a GroupsIO mailing list (idempotent create/update).
func (h *HandlerService) groupsioMailingListMemberPutHandler(message INatsMsg) error {
	return h.processGroupsIOMailingListMemberMessage(message, groupsioMailingListMemberPut)
}

// groupsioMailingListMemberRemoveHandler handles removing a member from a GroupsIO mailing list.
func (h *HandlerService) groupsioMailingListMemberRemoveHandler(message INatsMsg) error {
	return h.processGroupsIOMailingListMemberMessage(message, groupsioMailingListMemberRemove)
}
