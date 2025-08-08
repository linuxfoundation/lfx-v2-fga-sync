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

// TODO: update this payload schema to come from the project service
// Ticket https://linuxfoundation.atlassian.net/browse/LFXV2-147
type projectStub struct {
	UID                 string   `json:"uid"`
	Public              bool     `json:"public"`
	ParentUID           string   `json:"parent_uid"`
	Writers             []string `json:"writers"`
	Auditors            []string `json:"auditors"`
	MeetingCoordinators []string `json:"meeting_coordinators"`
}

// projectUpdateAccessHandler handles project access control updates.
func (h *HandlerService) projectUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling project access control update")

	// Parse the event data.
	project := new(projectStub)
	var err error
	err = json.Unmarshal(message.Data(), project)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Grab the project ID.
	if project.UID == "" {
		logger.ErrorContext(ctx, "project ID not found")
		return errors.New("project ID not found")
	}

	object := constants.ObjectTypeProject + project.UID

	// Build a list of tuples to sync.
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Convert the "public" attribute to a "user:*" relation.
	if project.Public {
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))
	}

	// Handle the parent relation.
	if project.ParentUID != "" {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeProject+project.ParentUID, constants.RelationParent, object),
		)
	}

	// Add each principal from the object as the corresponding relationship tuple
	// (as defined in the OpenFGA schema).
	for _, principal := range project.Writers {
		tuples = append(tuples, h.fgaService.TupleKey(constants.ObjectTypeUser+principal, constants.RelationWriter, object))
	}
	for _, principal := range project.Auditors {
		tuples = append(tuples, h.fgaService.TupleKey(constants.ObjectTypeUser+principal, constants.RelationAuditor, object))
	}
	for _, principal := range project.MeetingCoordinators {
		tuples = append(
			tuples,
			h.fgaService.TupleKey(constants.ObjectTypeUser+principal, constants.RelationMeetingCoordinator, object),
		)
	}

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

	if message.Reply() != "" {
		// Send a reply if an inbox was provided.
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, "sent project access control update response")
	}

	return nil
}

// projectDeleteAllAccessHandler handles project access control deletions.
func (h *HandlerService) projectDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeProject, "project")
}
