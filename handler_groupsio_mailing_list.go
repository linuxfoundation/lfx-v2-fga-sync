// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync MailingList.
package main

import (
	"context"
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

// groupsIOMailingListUpdateAccessHandler handles groups.io MailingList access control updates.
func (h *HandlerService) groupsIOMailingListUpdateAccessHandler(message INatsMsg) error {
	ctx := context.Background()
	logger.With("subject", string(message.Subject())).InfoContext(ctx, "handling groups.io MailingList access control update")

	// Parse the event data.
	groupsIOMailingList := new(standardAccessStub)
	err := json.Unmarshal(message.Data(), groupsIOMailingList)
	if err != nil {
		logger.With(errKey, err).ErrorContext(context.Background(), "event data parse error")
		return err
	}

	return h.processStandardAccessUpdate(message, groupsIOMailingList)
}

// groupsIOMailingListDeleteAllAccessHandler handles groups.io MailingList access control deletions.
func (h *HandlerService) groupsIOMailingListDeleteAllAccessHandler(message INatsMsg) error {
	ctx := context.Background()
	logger.With("subject", string(message.Subject())).InfoContext(ctx, "handling groups.io MailingList access control deletion")

	return h.processDeleteAllAccessMessage(message, constants.ObjectTypeGroupsIOMailingList, "groupsio_MailingList")
}
