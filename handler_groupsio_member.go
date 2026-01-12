// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

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
