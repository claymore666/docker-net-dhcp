// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gorilla/handlers"
)

func TestWriteAccessLog_DoesNotPanic(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/Plugin.Health", nil)
	u, err := url.Parse("/Plugin.Health")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	WriteAccessLog(&bytes.Buffer{}, handlers.LogFormatterParams{
		Request:    req,
		URL:        *u,
		StatusCode: 200,
		Size:       42,
	})
}
