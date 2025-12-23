// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"testing"
)

func TestPolicy_Validate(t *testing.T) {
	tests := []struct {
		name    string
		policy  Policy
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid policy",
			policy: Policy{
				Name:     "visibility_policy",
				Value:    "basic_profile",
				Relation: "allows_basic_profile",
			},
			wantErr: false,
		},
		{
			name: "empty name",
			policy: Policy{
				Name:     "",
				Value:    "basic_profile",
				Relation: "allows_basic_profile",
			},
			wantErr: true,
			errMsg:  "policy name cannot be empty",
		},
		{
			name: "empty value",
			policy: Policy{
				Name:     "visibility_policy",
				Value:    "",
				Relation: "allows_basic_profile",
			},
			wantErr: true,
			errMsg:  "policy value cannot be empty",
		},
		{
			name: "empty relation",
			policy: Policy{
				Name:     "visibility_policy",
				Value:    "basic_profile",
				Relation: "",
			},
			wantErr: true,
			errMsg:  "policy relation cannot be empty",
		},
		{
			name: "all fields empty",
			policy: Policy{
				Name:     "",
				Value:    "",
				Relation: "",
			},
			wantErr: true,
			errMsg:  "policy name cannot be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Policy.Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && err.Error() != tt.errMsg {
				t.Errorf("Policy.Validate() error message = %v, want %v", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestPolicy_ObjectID(t *testing.T) {
	tests := []struct {
		name   string
		policy Policy
		want   string
	}{
		{
			name: "visibility policy",
			policy: Policy{
				Name:  "visibility_policy",
				Value: "basic_profile",
			},
			want: "visibility_policy:basic_profile",
		},
		{
			name: "access policy",
			policy: Policy{
				Name:  "access_policy",
				Value: "admin",
			},
			want: "access_policy:admin",
		},
		{
			name: "empty values",
			policy: Policy{
				Name:  "",
				Value: "",
			},
			want: ":",
		},
		{
			name: "special characters",
			policy: Policy{
				Name:  "policy-name",
				Value: "value_123",
			},
			want: "policy-name:value_123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.ObjectID(); got != tt.want {
				t.Errorf("Policy.ObjectID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPolicy_UserRelation(t *testing.T) {
	tests := []struct {
		name     string
		policy   Policy
		objectID string
		relation string
		want     string
	}{
		{
			name:     "committee member relation",
			policy:   Policy{},
			objectID: "committee:123",
			relation: "member",
			want:     "committee:123#member",
		},
		{
			name:     "project viewer relation",
			policy:   Policy{},
			objectID: "project:abc-def-ghi",
			relation: "viewer",
			want:     "project:abc-def-ghi#viewer",
		},
		{
			name:     "team owner relation",
			policy:   Policy{},
			objectID: "team:xyz",
			relation: "owner",
			want:     "team:xyz#owner",
		},
		{
			name:     "empty objectID",
			policy:   Policy{},
			objectID: "",
			relation: "member",
			want:     "#member",
		},
		{
			name:     "empty relation",
			policy:   Policy{},
			objectID: "committee:123",
			relation: "",
			want:     "committee:123#",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.UserRelation(tt.objectID, tt.relation); got != tt.want {
				t.Errorf("Policy.UserRelation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPolicy_Integration(t *testing.T) {
	// Test that all methods work together correctly
	policy := Policy{
		Name:     "visibility_policy",
		Value:    "basic_profile",
		Relation: "allows_basic_profile",
	}

	// Validate the policy
	if err := policy.Validate(); err != nil {
		t.Errorf("Policy.Validate() failed for valid policy: %v", err)
	}

	// Get object ID
	objectID := policy.ObjectID()
	expectedObjectID := "visibility_policy:basic_profile"
	if objectID != expectedObjectID {
		t.Errorf("Policy.ObjectID() = %v, want %v", objectID, expectedObjectID)
	}

	// Get user relation
	committeeID := "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd"
	userRelation := policy.UserRelation(committeeID, "member")
	expectedUserRelation := "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd#member"
	if userRelation != expectedUserRelation {
		t.Errorf("Policy.UserRelation() = %v, want %v", userRelation, expectedUserRelation)
	}
}
