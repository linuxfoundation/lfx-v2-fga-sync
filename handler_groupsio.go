// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

// ============================================================================
// GroupsIO Service Handlers
// ============================================================================

// groupsIOServiceUpdateAccessHandler handles groups.io service access control updates.
func (h *HandlerService) groupsIOServiceUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()
	logger.With("message", string(message.Data())).InfoContext(ctx, "handling groups.io service access control update")

	// Parse the event data.
	groupsIOService := new(standardAccessStub)
	err := json.Unmarshal(message.Data(), groupsIOService)
	if err != nil {
		logger.With(errKey, err).ErrorContext(context.Background(), "event data parse error")
		return err
	}

	return h.processStandardAccessUpdate(message, groupsIOService)
}

// groupsIOServiceDeleteAllAccessHandler handles groups.io service access control deletions.
func (h *HandlerService) groupsIOServiceDeleteAllAccessHandler(message INatsMsg) error {
	ctx := context.Background()
	logger.With("message", string(message.Data())).InfoContext(ctx, "handling groups.io service access control deletion")

	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeGroupsIOService, "groupsio_service")
}

// ============================================================================
// GroupsIO Mailing List Handlers
// ============================================================================

// groupsIOMailingListUpdateAccessHandler handles groups.io mailing list access control updates.
func (h *HandlerService) groupsIOMailingListUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()
	logger.With("subject", message.Subject()).InfoContext(ctx, "handling groups.io mailing list access control update")

	// Parse the event data.
	groupsIOMailingList := new(standardAccessStub)
	err := json.Unmarshal(message.Data(), groupsIOMailingList)
	if err != nil {
		logger.With(errKey, err).ErrorContext(context.Background(), "event data parse error")
		return err
	}

	return h.processStandardAccessUpdate(message, groupsIOMailingList)
}

// groupsIOMailingListDeleteAllAccessHandler handles groups.io mailing list access control deletions.
func (h *HandlerService) groupsIOMailingListDeleteAllAccessHandler(message INatsMsg) error {
	ctx := context.Background()
	logger.With("subject", message.Subject()).InfoContext(ctx, "handling groups.io mailing list access control deletion")

	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeGroupsIOMailingList, "groupsio_mailing_list")
}

// ============================================================================
// GroupsIO Mailing List Member Handlers
// ============================================================================

// groupsIOMailingListMemberStub represents the structure of GroupsIO mailing list member data for FGA sync.
type groupsIOMailingListMemberStub struct {
	// UID is the mailing list member ID.
	UID string `json:"uid"`
	// Username is the username (i.e. LFID) of the member. This is the identity of the user object in FGA.
	Username string `json:"username"`
	// MailingListUID is the mailing list ID for the mailing list the member belongs to.
	MailingListUID string `json:"mailing_list_uid"`
}

// groupsIOMailingListMemberPutHandler handles putting a member to a GroupsIO mailing list (idempotent create/update).
func (h *HandlerService) groupsIOMailingListMemberPutHandler(message INatsMsg) error {
	// Parse GroupsIO-specific format
	groupsIOMember := new(groupsIOMailingListMemberStub)
	if err := json.Unmarshal(message.Data(), groupsIOMember); err != nil {
		return err
	}

	// Convert to generic format
	genericMember := &memberOperationStub{
		Username:  groupsIOMember.Username,
		ObjectUID: groupsIOMember.MailingListUID,
	}

	config := memberOperationConfig{
		objectTypePrefix: constants.ObjectTypeGroupsIOMailingList,
		objectTypeName:   "groupsio mailing list",
		relation:         constants.RelationMember,
	}

	return h.processMemberOperation(message, genericMember, memberOperationPut, config)
}

// groupsIOMailingListMemberRemoveHandler handles removing a member from a GroupsIO mailing list.
func (h *HandlerService) groupsIOMailingListMemberRemoveHandler(message INatsMsg) error {
	// Parse GroupsIO-specific format
	groupsIOMember := new(groupsIOMailingListMemberStub)
	if err := json.Unmarshal(message.Data(), groupsIOMember); err != nil {
		return err
	}

	// Convert to generic format
	genericMember := &memberOperationStub{
		Username:  groupsIOMember.Username,
		ObjectUID: groupsIOMember.MailingListUID,
	}

	config := memberOperationConfig{
		objectTypePrefix: constants.ObjectTypeGroupsIOMailingList,
		objectTypeName:   "groupsio mailing list",
		relation:         constants.RelationMember,
	}

	return h.processMemberOperation(message, genericMember, memberOperationRemove, config)
}
