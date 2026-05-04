// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package types defines shared message types for the fga-sync service.
package types

import (
	"encoding/json"
	"testing"
)

func TestGenericFGAMessage_UnmarshalData_Success(t *testing.T) {
	accessData := GenericAccessData{
		UID:    "committee-123",
		Public: true,
		Relations: map[string][]string{
			"member": {"user-alice"},
		},
	}

	raw, _ := json.Marshal(accessData)
	var dataAny any
	_ = json.Unmarshal(raw, &dataAny)

	msg := GenericFGAMessage{
		ObjectType: "committee",
		Operation:  "update_access",
		Data:       dataAny,
	}

	var decoded GenericAccessData
	if err := msg.UnmarshalData(&decoded); err != nil {
		t.Fatalf("UnmarshalData returned unexpected error: %v", err)
	}
	if decoded.UID != accessData.UID {
		t.Errorf("UID: got %q, want %q", decoded.UID, accessData.UID)
	}
	if decoded.Public != accessData.Public {
		t.Errorf("Public: got %v, want %v", decoded.Public, accessData.Public)
	}
	if len(decoded.Relations["member"]) != 1 || decoded.Relations["member"][0] != "user-alice" {
		t.Errorf("Relations: got %v, want member=[user-alice]", decoded.Relations)
	}
}

func TestGenericFGAMessage_UnmarshalData_MemberData(t *testing.T) {
	memberData := GenericMemberData{
		UID:                   "meeting-456",
		Username:              "user-bob",
		Relations:             []string{"host"},
		MutuallyExclusiveWith: []string{"participant"},
	}

	raw, _ := json.Marshal(memberData)
	var dataAny any
	_ = json.Unmarshal(raw, &dataAny)

	msg := GenericFGAMessage{
		ObjectType: "meeting",
		Operation:  "member_put",
		Data:       dataAny,
	}

	var decoded GenericMemberData
	if err := msg.UnmarshalData(&decoded); err != nil {
		t.Fatalf("UnmarshalData returned unexpected error: %v", err)
	}
	if decoded.Username != memberData.Username {
		t.Errorf("Username: got %q, want %q", decoded.Username, memberData.Username)
	}
	if len(decoded.Relations) != 1 || decoded.Relations[0] != "host" {
		t.Errorf("Relations: got %v, want [host]", decoded.Relations)
	}
	if len(decoded.MutuallyExclusiveWith) != 1 || decoded.MutuallyExclusiveWith[0] != "participant" {
		t.Errorf("MutuallyExclusiveWith: got %v, want [participant]", decoded.MutuallyExclusiveWith)
	}
}

func TestGenericFGAMessage_UnmarshalData_NilData(t *testing.T) {
	msg := GenericFGAMessage{
		ObjectType: "committee",
		Operation:  "update_access",
		Data:       nil,
	}

	var decoded GenericAccessData
	if err := msg.UnmarshalData(&decoded); err != nil {
		t.Fatalf("UnmarshalData with nil Data returned unexpected error: %v", err)
	}
	if decoded.UID != "" {
		t.Errorf("expected zero-value UID, got %q", decoded.UID)
	}
}
