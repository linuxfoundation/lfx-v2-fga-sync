// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync Member.
package main

import (
	"context"
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

// groupsIOMemberUpdateAccessHandler handles groups.io Member access control updates.
func (h *HandlerService) groupsIOMemberUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()
	logger.With("subject", message.Subject()).InfoContext(ctx, "handling groups.io Member access control update")

	// Parse the event data.
	groupsIOMember := new(standardAccessStub)
	err := json.Unmarshal(message.Data(), groupsIOMember)
	if err != nil {
		logger.With(errKey, err).ErrorContext(context.Background(), "event data parse error")
		return err
	}

	return h.processStandardAccessUpdate(message, groupsIOMember)
}

// groupsIOMemberDeleteAllAccessHandler handles groups.io Member access control deletions.
func (h *HandlerService) groupsIOMemberDeleteAllAccessHandler(message INatsMsg) error {
	ctx := context.Background()
	logger.With("subject", message.Subject()).InfoContext(ctx, "handling groups.io Member access control deletion")

	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeGroupsIOMember, "groupsio_Member")
}
