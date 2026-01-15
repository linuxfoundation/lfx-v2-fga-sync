// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

type committeeStub struct {
	UID        string              `json:"uid"`
	ObjectType string              `json:"object_type"`
	Public     bool                `json:"public"`
	Relations  map[string][]string `json:"relations"`
	References map[string]string   `json:"references"`
}

// committeeUpdateAccessHandler handles committee access control updates.
func (h *HandlerService) committeeUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse committee-specific format
	committee := new(committeeStub)
	err := json.Unmarshal(message.Data(), committee)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Convert to standardAccessStub format
	standardAccess := &standardAccessStub{
		UID:        committee.UID,
		ObjectType: committee.ObjectType,
		Public:     committee.Public,
		Relations:  committee.Relations,
		References: make(map[string][]string),
	}

	// Convert References from map[string]string to map[string][]string
	for key, value := range committee.References {
		standardAccess.References[key] = []string{value}
	}

	// Use the generic handler
	return h.processStandardAccessUpdate(message, standardAccess)
}

// committeeDeleteAllAccessHandler handles committee access control deletions.
func (h *HandlerService) committeeDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeCommittee, "committee")
}

// committeeMemberStub represents the structure of committee member data for FGA sync.
type committeeMemberStub struct {
	// Username is the username (i.e. LFID) of the member. This is the identity of the user object in FGA.
	Username string `json:"username"`
	// CommitteeUID is the committee ID for the committee the member belongs to.
	CommitteeUID string `json:"committee_uid"`
}

// committeeMemberPutHandler handles putting a member to a committee (idempotent create/update).
func (h *HandlerService) committeeMemberPutHandler(message INatsMsg) error {
	// Parse committee-specific format
	committeeMember := new(committeeMemberStub)
	if err := json.Unmarshal(message.Data(), committeeMember); err != nil {
		return err
	}

	// Convert to generic format
	genericMember := &memberOperationStub{
		Username:  committeeMember.Username,
		ObjectUID: committeeMember.CommitteeUID,
	}

	config := memberOperationConfig{
		objectTypePrefix: constants.ObjectTypeCommittee,
		objectTypeName:   "committee",
		relation:         constants.RelationMember,
	}

	return h.processMemberOperation(message, genericMember, memberOperationPut, config)
}

// committeeMemberRemoveHandler handles removing a member from a committee.
func (h *HandlerService) committeeMemberRemoveHandler(message INatsMsg) error {
	// Parse committee-specific format
	committeeMember := new(committeeMemberStub)
	if err := json.Unmarshal(message.Data(), committeeMember); err != nil {
		return err
	}

	// Convert to generic format
	genericMember := &memberOperationStub{
		Username:  committeeMember.Username,
		ObjectUID: committeeMember.CommitteeUID,
	}

	config := memberOperationConfig{
		objectTypePrefix: constants.ObjectTypeCommittee,
		objectTypeName:   "committee",
		relation:         constants.RelationMember,
	}

	return h.processMemberOperation(message, genericMember, memberOperationRemove, config)
}
