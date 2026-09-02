// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubNatsChecker implements natsReadyChecker for unit tests.
type stubNatsChecker struct {
	connected bool
	draining  bool
}

func (s *stubNatsChecker) IsConnected() bool { return s.connected }
func (s *stubNatsChecker) IsDraining() bool  { return s.draining }

func TestReadyzHandler(t *testing.T) {
	connected := &stubNatsChecker{connected: true, draining: false}
	disconnected := &stubNatsChecker{connected: false, draining: false}
	draining := &stubNatsChecker{connected: true, draining: true}

	tests := []struct {
		name        string
		ready       bool
		natsChecker natsReadyChecker
		wantStatus  int
		wantBody    string
	}{
		{
			name:        "pre-startup: ready flag not set",
			ready:       false,
			natsChecker: nil,
			wantStatus:  http.StatusServiceUnavailable,
			wantBody:    "not ready\n",
		},
		{
			name:        "shutdown: ready flag cleared before drain",
			ready:       false,
			natsChecker: connected,
			wantStatus:  http.StatusServiceUnavailable,
			wantBody:    "not ready\n",
		},
		{
			name:        "ready: fully initialized and NATS connected",
			ready:       true,
			natsChecker: connected,
			wantStatus:  http.StatusOK,
			wantBody:    "OK\n",
		},
		{
			name:        "ready flag set but NATS disconnected at runtime",
			ready:       true,
			natsChecker: disconnected,
			wantStatus:  http.StatusServiceUnavailable,
			wantBody:    "NATS connection not ready\n",
		},
		{
			name:        "ready flag set but NATS draining",
			ready:       true,
			natsChecker: draining,
			wantStatus:  http.StatusServiceUnavailable,
			wantBody:    "NATS connection not ready\n",
		},
		{
			name:        "ready flag set but natsChecker nil",
			ready:       true,
			natsChecker: nil,
			wantStatus:  http.StatusServiceUnavailable,
			wantBody:    "NATS connection not ready\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &server{natsChecker: tt.natsChecker}
			srv.ready.Store(tt.ready)

			rec := httptest.NewRecorder()
			srv.readyzHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

func TestLivezHandler(t *testing.T) {
	srv := &server{}
	rec := httptest.NewRecorder()
	srv.livezHandler(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "OK\n" {
		t.Errorf("body = %q, want %q", got, "OK\n")
	}
}
