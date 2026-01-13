// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"

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

// toStandardAccessStub converts a projectStub to the standard access control format
func (p *projectStub) toStandardAccessStub() *standardAccessStub {
	stub := &standardAccessStub{
		UID:        p.UID,
		ObjectType: "project",
		Public:     p.Public,
		Relations:  make(map[string][]string),
		References: make(map[string][]string),
	}

	// Convert parent_uid to References
	if p.ParentUID != "" {
		stub.References[constants.RelationParent] = []string{p.ParentUID}
	}

	// Convert role arrays to Relations
	if len(p.Writers) > 0 {
		stub.Relations[constants.RelationWriter] = p.Writers
	}
	if len(p.Auditors) > 0 {
		stub.Relations[constants.RelationAuditor] = p.Auditors
	}
	if len(p.MeetingCoordinators) > 0 {
		stub.Relations[constants.RelationMeetingCoordinator] = p.MeetingCoordinators
	}

	return stub
}

// projectUpdateAccessHandler handles project access control updates.
func (h *HandlerService) projectUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()

	// Parse the event data as projectStub for backward compatibility
	project := new(projectStub)
	err := json.Unmarshal(message.Data(), project)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "event data parse error")
		return err
	}

	// Convert to standard access stub and use the generic handler
	standardAccess := project.toStandardAccessStub()
	return h.processStandardAccessUpdate(message, standardAccess)
}

// projectDeleteAllAccessHandler handles project access control deletions.
func (h *HandlerService) projectDeleteAllAccessHandler(message INatsMsg) error {
	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeProject, "project")
}
