// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

type meetingStub struct {
	UID        string   `json:"uid"`
	Public     bool     `json:"public"`
	ProjectUID string   `json:"project_uid"`
	Organizers []string `json:"organizers"`
	Committees []string `json:"committees"`
}

// toStandardAccessStub converts meetingStub to standardAccessStub format.
func (m *meetingStub) toStandardAccessStub() *standardAccessStub {
	stub := &standardAccessStub{
		UID:        m.UID,
		ObjectType: "meeting", // Without colon - processStandardAccessUpdate adds it
		Public:     m.Public,
		Relations:  make(map[string][]string),
		References: make(map[string][]string),
	}

	// Convert project_uid to References
	if m.ProjectUID != "" {
		stub.References[constants.RelationProject] = []string{m.ProjectUID}
	}

	// Convert committees to References
	if len(m.Committees) > 0 {
		stub.References[constants.RelationCommittee] = m.Committees
	}

	// Convert organizers to Relations
	if len(m.Organizers) > 0 {
		stub.Relations[constants.RelationOrganizer] = m.Organizers
	}

	return stub
}

// meetingUpdateAccessHandler handles meeting access control updates.
func (h *HandlerService) meetingUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse the event data.
	meeting := new(meetingStub)
	err := json.Unmarshal(message.Data(), meeting)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate project ID.
	if meeting.ProjectUID == "" {
		logger.ErrorContext(ctx, "meeting project ID not found")
		return errors.New("meeting project ID not found")
	}

	// Convert to standard format
	standardAccess := meeting.toStandardAccessStub()

	// Use the generic handler with excluded relations.
	// Exclude participant and host relations from deletion - these are managed by other messages.
	return h.processStandardAccessUpdate(
		message,
		standardAccess,
		constants.RelationParticipant,
		constants.RelationHost,
	)
}

// meetingDeleteAllAccessHandler handles deleting all tuples for a meeting object.
//
// This should only happen when a meeting is deleted.
func (h *HandlerService) meetingDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeMeeting, "meeting")
}

type registrantStub struct {
	// UID is the registrant ID for the user's registration on the meeting.
	UID string `json:"uid"`
	// Username is the username (i.e. LFID) of the registrant. This is the identity of the user object in FGA.
	Username string `json:"username"`
	// MeetingUID is the meeting ID for the meeting the registrant is registered for.
	MeetingUID string `json:"meeting_uid"`
	// Host determines whether the user should get host relation on the meeting
	Host bool `json:"host"`
}

// meetingRegistrantPutHandler handles putting a registrant to a meeting (idempotent create/update).
func (h *HandlerService) meetingRegistrantPutHandler(message INatsMsg) error {
	// Parse registrant-specific format
	registrant := new(registrantStub)
	if err := json.Unmarshal(message.Data(), registrant); err != nil {
		return err
	}

	// Convert to generic format
	genericMember := &memberOperationStub{
		Username:  registrant.Username,
		ObjectUID: registrant.MeetingUID,
	}

	// Determine the relation based on whether they're a host
	relation := constants.RelationParticipant
	if registrant.Host {
		relation = constants.RelationHost
	}

	// Configure with mutually exclusive relations
	// When setting a registrant as participant or host, the opposite relation should be removed
	config := memberOperationConfig{
		objectTypePrefix:      constants.ObjectTypeMeeting,
		objectTypeName:        "meeting",
		relation:              relation,
		mutuallyExclusiveWith: []string{constants.RelationParticipant, constants.RelationHost},
	}

	return h.processMemberOperation(message, genericMember, memberOperationPut, config)
}

// meetingRegistrantRemoveHandler handles removing a registrant from a meeting.
func (h *HandlerService) meetingRegistrantRemoveHandler(message INatsMsg) error {
	// Parse registrant-specific format
	registrant := new(registrantStub)
	if err := json.Unmarshal(message.Data(), registrant); err != nil {
		return err
	}

	// Convert to generic format
	genericMember := &memberOperationStub{
		Username:  registrant.Username,
		ObjectUID: registrant.MeetingUID,
	}

	// Determine the relation based on whether they're a host
	relation := constants.RelationParticipant
	if registrant.Host {
		relation = constants.RelationHost
	}

	// Configure with mutually exclusive relations
	config := memberOperationConfig{
		objectTypePrefix:      constants.ObjectTypeMeeting,
		objectTypeName:        "meeting",
		relation:              relation,
		mutuallyExclusiveWith: []string{constants.RelationParticipant, constants.RelationHost},
	}

	return h.processMemberOperation(message, genericMember, memberOperationRemove, config)
}

type meetingAttachmentStub struct {
	UID        string `json:"uid"`
	MeetingUID string `json:"meeting_uid"`
}

// meetingAttachmentUpdateAccessHandler handles meeting attachment access control updates.
func (h *HandlerService) meetingAttachmentUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling meeting attachment access control update")

	// Parse the event data
	attachment := new(meetingAttachmentStub)
	if err := json.Unmarshal(message.Data(), attachment); err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Validate required fields
	if attachment.UID == "" {
		logger.ErrorContext(ctx, "meeting attachment UID not found")
		return errors.New("meeting attachment UID not found")
	}
	if attachment.MeetingUID == "" {
		logger.ErrorContext(ctx, "meeting UID not found")
		return errors.New("meeting UID not found")
	}

	object := constants.ObjectTypeMeetingAttachment + attachment.UID

	// Build tuples - associate attachment with its meeting
	tuples := h.fgaService.NewTupleKeySlice(1)
	if attachment.MeetingUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeMeeting+attachment.MeetingUID, constants.RelationMeeting, object),
		)
	}

	// Sync tuples
	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, tuples)
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

	// Reply handling
	if message.Reply() != "" {
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, "sent meeting attachment access control update response")
	}

	return nil
}

// meetingAttachmentDeleteAccessHandler handles deleting all tuples for a meeting attachment object.
//
// This should happen when a meeting attachment is deleted.
func (h *HandlerService) meetingAttachmentDeleteAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeMeetingAttachment, "meeting attachment")
}
