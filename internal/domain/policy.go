// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import "fmt"

// Policy represents a fine-grained authorization policy.
type Policy struct {
	Name     string `json:"name"`
	Relation string `json:"relation"`
	Value    string `json:"value"`
}

// Validate checks if the policy has all required fields.
// Returns an error if any field is empty.
func (p Policy) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("policy name cannot be empty")
	}
	if p.Value == "" {
		return fmt.Errorf("policy value cannot be empty")
	}
	if p.Relation == "" {
		return fmt.Errorf("policy relation cannot be empty")
	}
	return nil
}

// ObjectID returns the OpenFGA object identifier for this policy.
// Format: policy.Name:policy.Value
// Example: "visibility_policy:basic_profile"
func (p Policy) ObjectID() string {
	return fmt.Sprintf("%s:%s", p.Name, p.Value)
}

// UserRelation returns the user relation string for a given object.
// Format: objectID#relation
// Example: "committee:123#member"
func (p Policy) UserRelation(objectID, relation string) string {
	return fmt.Sprintf("%s#%s", objectID, relation)
}
