// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

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
