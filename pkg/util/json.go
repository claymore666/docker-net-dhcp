package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	log "github.com/sirupsen/logrus"
)

// JSONResponse Sends a JSON payload in response to a HTTP request.
// The payload is encoded into a buffer first so that, on encoding failure,
// we can still send a clean HTTP 500 instead of a garbled response with a
// half-flushed body and a no-op second WriteHeader call.
func JSONResponse(w http.ResponseWriter, v interface{}, statusCode int) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		log.WithField("err", err).Error("Failed to serialize JSON payload")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Failed to serialize JSON payload\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(buf.Bytes())
}

type jsonError struct {
	Message string `json:"Err"`
}

// JSONErrResponse Sends an `error` as a JSON object with a `message` property
func JSONErrResponse(w http.ResponseWriter, err error, statusCode int) {
	log.WithError(err).Error("Error while processing request")

	w.Header().Set("Content-Type", "application/problem+json")
	if statusCode == 0 {
		statusCode = ErrToStatus(err)
	}
	w.WriteHeader(statusCode)

	if encErr := json.NewEncoder(w).Encode(jsonError{err.Error()}); encErr != nil {
		log.WithError(encErr).Debug("Failed to write JSON error body")
	}
}

// ParseJSONOrErrorResponse decodes the request body as JSON into v.
// On failure it ALSO writes a 400 JSON error response to w; the
// caller is expected to early-return on a non-nil error and not
// touch w again. The verbose name is deliberate: the prior name
// (ParseJSONBody) read as a pure parse, but the function quietly
// took over response writing — a future caller writing the
// obvious-looking
//
//	if err := ParseJSONBody(&req, w, r); err != nil {
//	    JSONErrResponse(w, err, ...); return
//	}
//
// would double-write headers. This name makes the response-writing
// side-effect impossible to overlook at the call site.
func ParseJSONOrErrorResponse(v interface{}, w http.ResponseWriter, r *http.Request) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		JSONErrResponse(w, fmt.Errorf("failed to parse request body: %w", err), http.StatusBadRequest)
		return err
	}
	return nil
}
