// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// sync_global_groups syncs LDAP global groups to corresponding OpenFGA team
// member tuples, adding and removing "user:" relations as needed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Configuration read from environment variables.
var (
	ldapRestProxy      = os.Getenv("LDAP_REST_PROXY")
	oauthTokenEndpoint = os.Getenv("OAUTH_TOKEN_ENDPOINT")
	clientID           = os.Getenv("CLIENT_ID")
	clientSecret       = os.Getenv("CLIENT_SECRET")
	openfgaURL         = os.Getenv("OPENFGA_API_URL")
	openfgaStoreID     = os.Getenv("OPENFGA_STORE_ID")
	debug              = isTruthy(os.Getenv("DEBUG"))
	dryRun             = isTruthy(os.Getenv("DRYRUN"))
)

// isTruthy returns true if the value is a common truthy string.
func isTruthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "t", "y", "true", "yes":
		return true
	}
	return false
}

// token holds an OAuth2 client_credentials token.
type token struct {
	AccessToken string  `json:"access_token"`
	ExpiresIn   float64 `json:"expires_in"`
	expiresAt   time.Time
}

var (
	currentToken *token
	httpClient   = &http.Client{Timeout: 30 * time.Second}
)

// fetchToken retrieves a new client_credentials token from the OAuth token endpoint.
func fetchToken(ctx context.Context) (*token, error) {
	audience := ldapRestProxy
	body := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"audience":      {audience},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenEndpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token request returned %d: %s", resp.StatusCode, data)
	}
	var t token
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	t.expiresAt = time.Now().Add(time.Duration(t.ExpiresIn-30) * time.Second)
	return &t, nil
}

// ldapBearerToken returns a valid bearer token for the LDAP REST proxy.
func ldapBearerToken(ctx context.Context) (string, error) {
	if currentToken == nil || time.Now().After(currentToken.expiresAt) {
		t, err := fetchToken(ctx)
		if err != nil {
			return "", err
		}
		currentToken = t
	}
	return currentToken.AccessToken, nil
}

// ldapGroupMembers returns a map of lowercase username → original username for
// all members of the given LDAP group.
func ldapGroupMembers(ctx context.Context, group string) (map[string]string, error) {
	bearer, err := ldapBearerToken(ctx)
	if err != nil {
		return nil, err
	}
	reqURL := strings.TrimRight(ldapRestProxy, "/") + "/groups/" + url.PathEscape(group)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("LDAP request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("LDAP returned %d: %s", resp.StatusCode, data)
	}

	var usernames []string
	if err := json.NewDecoder(resp.Body).Decode(&usernames); err != nil {
		return nil, fmt.Errorf("decode LDAP response: %w", err)
	}

	result := make(map[string]string, len(usernames))
	for _, u := range usernames {
		result[strings.ToLower(u)] = u
	}
	return result, nil
}

// fgaReadRequest is the request body for the OpenFGA Read API.
type fgaReadRequest struct {
	TupleKey          fgaTupleKey `json:"tuple_key"`
	PageSize          int         `json:"page_size"`
	ContinuationToken string      `json:"continuation_token,omitempty"`
}

// fgaTupleKey identifies a relationship triple.
type fgaTupleKey struct {
	User     string `json:"user,omitempty"`
	Relation string `json:"relation,omitempty"`
	Object   string `json:"object,omitempty"`
}

// fgaReadResponse is the response body from the OpenFGA Read API.
type fgaReadResponse struct {
	Tuples []struct {
		Key fgaTupleKey `json:"key"`
	} `json:"tuples"`
	ContinuationToken string `json:"continuation_token"`
}

// fgaTeamMembers returns a map of lowercase username → original username for
// all "user:" subjects with the "member" relation to the given team object.
func fgaTeamMembers(ctx context.Context, teamObject string) (map[string]string, error) {
	if openfgaStoreID == "" {
		return nil, fmt.Errorf("OPENFGA_STORE_ID is required")
	}
	endpoint := fmt.Sprintf("%s/stores/%s/read", strings.TrimRight(openfgaURL, "/"), openfgaStoreID)

	result := make(map[string]string)
	var contToken string

	for {
		reqBody := fgaReadRequest{
			TupleKey: fgaTupleKey{
				Relation: "member",
				Object:   teamObject,
			},
			PageSize: 100,
		}
		if contToken != "" {
			reqBody.ContinuationToken = contToken
		}

		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("OpenFGA request: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OpenFGA returned %d: %s", resp.StatusCode, body)
		}

		var fgaResp fgaReadResponse
		if err := json.Unmarshal(body, &fgaResp); err != nil {
			return nil, fmt.Errorf("decode OpenFGA response: %w", err)
		}

		for _, t := range fgaResp.Tuples {
			user := t.Key.User
			// Only consider "user:" subjects; ignore wildcards or other types.
			// Strip the "user:" prefix, then strip the "auth0|" prefix added by
			// usernameToSub so the key matches the raw LDAP username for diffing.
			if after, ok := strings.CutPrefix(user, "user:"); ok {
				username, _ := strings.CutPrefix(after, "auth0|")
				result[strings.ToLower(username)] = after
			}
		}

		if fgaResp.ContinuationToken == "" {
			break
		}
		contToken = fgaResp.ContinuationToken
	}

	return result, nil
}

// fgaWriteRequest is the request body for the OpenFGA Write API.
type fgaWriteRequest struct {
	Writes  *fgaTupleKeys `json:"writes,omitempty"`
	Deletes *fgaTupleKeys `json:"deletes,omitempty"`
}

// fgaTupleKeys is a list of tuple keys for the OpenFGA Write API.
type fgaTupleKeys struct {
	TupleKeys []fgaTupleKey `json:"tuple_keys"`
}

// fgaWrite writes or deletes tuples in OpenFGA.
func fgaWrite(ctx context.Context, writes, deletes []fgaTupleKey) error {
	endpoint := fmt.Sprintf("%s/stores/%s/write", strings.TrimRight(openfgaURL, "/"), openfgaStoreID)

	// OpenFGA write API accepts at most 100 tuples per request; batch if needed.
	batch := func(keys []fgaTupleKey, isWrite bool) error {
		for i := 0; i < len(keys); i += 100 {
			end := i + 100
			if end > len(keys) {
				end = len(keys)
			}
			chunk := keys[i:end]
			reqBody := fgaWriteRequest{}
			if isWrite {
				reqBody.Writes = &fgaTupleKeys{TupleKeys: chunk}
			} else {
				reqBody.Deletes = &fgaTupleKeys{TupleKeys: chunk}
			}
			data, err := json.Marshal(reqBody)
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("OpenFGA write request: %w", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("OpenFGA write returned %d: %s", resp.StatusCode, body)
			}
		}
		return nil
	}

	if len(writes) > 0 {
		if err := batch(writes, true); err != nil {
			return err
		}
	}
	if len(deletes) > 0 {
		if err := batch(deletes, false); err != nil {
			return err
		}
	}
	return nil
}

// usernameRegexp matches valid LDAP usernames that can be converted to Auth0
// subject IDs by prepending "auth0|".
var usernameRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,58}[A-Za-z0-9]$`)

// usernameToSub converts an LDAP username to an Auth0 subject ID. It returns
// an error if the username does not match the expected pattern, in which case
// the caller should skip the user.
func usernameToSub(username string) (string, error) {
	if !usernameRegexp.MatchString(username) {
		return "", fmt.Errorf("username %q does not match expected pattern; skipping", username)
	}
	return "auth0|" + username, nil
}

// syncGroup syncs the members of an LDAP group to the corresponding OpenFGA
// team object, adding and removing tuples as needed.
func syncGroup(ctx context.Context, ldapGroup, fgaTeamObject string) error {
	slog.DebugContext(ctx, "fetching members from LDAP", "group", ldapGroup)
	ldapMembers, err := ldapGroupMembers(ctx, ldapGroup)
	if err != nil {
		return fmt.Errorf("LDAP error for %s: %w", ldapGroup, err)
	}
	slog.DebugContext(ctx, "fetched LDAP group members",
		"group", ldapGroup,
		"count", len(ldapMembers))

	slog.DebugContext(ctx, "fetching members from OpenFGA", "object", fgaTeamObject)
	fgaMembers, err := fgaTeamMembers(ctx, fgaTeamObject)
	if err != nil {
		return fmt.Errorf("OpenFGA error for %s: %w", fgaTeamObject, err)
	}
	slog.DebugContext(ctx, "fetched OpenFGA team members",
		"object", fgaTeamObject,
		"count", len(fgaMembers))

	toAdd := make([]fgaTupleKey, 0, len(ldapMembers))
	toRemove := make([]fgaTupleKey, 0, len(fgaMembers))

	for lower, original := range ldapMembers {
		if _, ok := fgaMembers[lower]; !ok {
			sub, err := usernameToSub(original)
			if err != nil {
				slog.WarnContext(ctx, "skipping user", "username", original, "reason", err.Error())
				continue
			}
			toAdd = append(toAdd, fgaTupleKey{
				User:     "user:" + sub,
				Relation: "member",
				Object:   fgaTeamObject,
			})
		}
	}
	for lower, original := range fgaMembers {
		if _, ok := ldapMembers[lower]; !ok {
			toRemove = append(toRemove, fgaTupleKey{
				User:     "user:" + original,
				Relation: "member",
				Object:   fgaTeamObject,
			})
		}
	}

	if len(toAdd) == 0 && len(toRemove) == 0 {
		slog.DebugContext(ctx, "already in sync", "object", fgaTeamObject)
		return nil
	}

	for _, t := range toAdd {
		slog.InfoContext(ctx, "adding member", "user", t.User, "object", fgaTeamObject)
	}
	for _, t := range toRemove {
		slog.InfoContext(ctx, "removing member", "user", t.User, "object", fgaTeamObject)
	}

	if dryRun {
		slog.InfoContext(ctx, "dry run; skipping write",
			"object", fgaTeamObject,
			"would_add", len(toAdd),
			"would_remove", len(toRemove))
		return nil
	}

	if err := fgaWrite(ctx, toAdd, toRemove); err != nil {
		return fmt.Errorf("write error for %s: %w", fgaTeamObject, err)
	}
	slog.InfoContext(ctx, "sync complete",
		"object", fgaTeamObject,
		"added", len(toAdd),
		"removed", len(toRemove))
	return nil
}

func main() {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if ldapRestProxy == "" {
		slog.Error("LDAP_REST_PROXY is required")
		os.Exit(1)
	}
	if oauthTokenEndpoint == "" {
		slog.Error("OAUTH_TOKEN_ENDPOINT is required")
		os.Exit(1)
	}
	if clientID == "" {
		slog.Error("CLIENT_ID is required")
		os.Exit(1)
	}
	if clientSecret == "" {
		slog.Error("CLIENT_SECRET is required")
		os.Exit(1)
	}
	if openfgaURL == "" {
		slog.Error("OPENFGA_API_URL is required")
		os.Exit(1)
	}
	if openfgaStoreID == "" {
		slog.Error("OPENFGA_STORE_ID is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// groups maps LDAP group names to OpenFGA team object names.
	groups := [][2]string{
		{"lf-staff", "team:lf-staff"},
		{"lf-contractor", "team:lf-contractor"},
	}

	var failed bool
	for _, g := range groups {
		if err := syncGroup(ctx, g[0], g[1]); err != nil {
			slog.ErrorContext(ctx, "sync failed", "group", g[0], "error", err.Error())
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
