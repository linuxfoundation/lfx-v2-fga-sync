// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package types provides shared message type definitions for the fga-sync service.
// Import this package from any LFX service that publishes FGA sync events so that
// the message envelope and data payloads stay consistent across the platform.
package types

import "encoding/json"

// GenericFGAMessage is the universal message format for all FGA operations.
// This allows clients to send resource-agnostic messages without needing
// to know about resource-specific NATS subjects or message formats.
type GenericFGAMessage struct {
	ObjectType string `json:"object_type"` // e.g., "committee", "project", "meeting"
	Operation  string `json:"operation"`   // e.g., "update_access", "member_put"
	Data       any    `json:"data"`        // Operation-specific payload
}

// UnmarshalData unmarshals the Data field into a specific type.
// Use this on the consumer side (fga-sync) to decode the operation payload.
func (m *GenericFGAMessage) UnmarshalData(v any) error {
	b, err := json.Marshal(m.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// GenericAccessData is the Data payload for update_access operations.
// This is a full sync — any relations not listed (and not excluded) will be removed.
type GenericAccessData struct {
	UID              string              `json:"uid"`
	Public           bool                `json:"public"`
	Relations        map[string][]string `json:"relations"`         // relation_name → [usernames]
	References       map[string][]string `json:"references"`        // relation_name → [object_uids]
	ExcludeRelations []string            `json:"exclude_relations"` // relations managed elsewhere
}

// GenericDeleteData is the Data payload for delete_access operations.
type GenericDeleteData struct {
	UID string `json:"uid"`
}

// GenericMemberData is the Data payload for member_put and member_remove operations.
// Supports multiple relations for a single user, enabling atomic updates.
type GenericMemberData struct {
	UID                   string   `json:"uid"`
	Username              string   `json:"username"`
	Relations             []string `json:"relations"`               // relations to add or remove
	MutuallyExclusiveWith []string `json:"mutually_exclusive_with"` // on member_put: remove these
}
