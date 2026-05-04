package util

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gorilla/handlers"
)

// TestWriteAccessLog_DoesNotPanic exercises the access-log adapter
// over the field shape that gorilla/handlers passes in. It's a thin
// wrapper around logrus.Tracef but the field layout is what makes
// it pluggable into handlers.CustomLoggingHandler — a refactor that
// changed the signature would silently break the access-log path
// at server startup.
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
