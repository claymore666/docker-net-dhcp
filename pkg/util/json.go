// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	log "github.com/sirupsen/logrus"
)

// JSONResponse sends v as JSON with statusCode, encoded first so an encoding failure still sends a clean 500.
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

// JSONErrResponse sends err as a JSON object and logs at Error for 5xx, Warn for 4xx and Info otherwise (#88).
func JSONErrResponse(w http.ResponseWriter, err error, statusCode int) {
	if statusCode == 0 {
		statusCode = ErrToStatus(err)
	}

	entry := log.WithError(err).WithField("status", statusCode)
	switch {
	case statusCode >= 500:
		entry.Error("Error while processing request")
	case statusCode >= 400:
		entry.Warn("Caller error while processing request")
	default:
		entry.Info("Non-success response")
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(statusCode)

	if encErr := json.NewEncoder(w).Encode(jsonError{err.Error()}); encErr != nil {
		log.WithError(encErr).Debug("Failed to write JSON error body")
	}
}

// ParseJSONOrErrorResponse decodes the body into v and, on failure, has already written the 400 response to w.
func ParseJSONOrErrorResponse(v interface{}, w http.ResponseWriter, r *http.Request) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		JSONErrResponse(w, explainRequestBody(err), http.StatusBadRequest)
		return err
	}
	return nil
}

// The daemon hands the same drained buffer to every attempt of a call (moby pkg/plugins/client.go, callWithRetry), so
// a re-send after its client timeout arrives with no body. The daemon does not pass `--timeout` to the plugin, so its
// budgets are sized to the 30s default (plugin.pluginCallBudget) and only a lower value changes anything, for the
// worse (#110). Measured in integration run 34600486961: a reservation held past a 5s timeout surfaced as "failed to
// parse request body: EOF". io.ErrUnexpectedEOF is a broken connection and keeps the generic text.

// explainRequestBody names the one thing an empty body means on the plugin socket.
func explainRequestBody(err error) error {
	if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("this request arrived with no body. On the plugin socket that means "+
			"the daemon re-sent a call whose body it had already spent: the first attempt was "+
			"still running when the plugin call timeout expired (set with `--timeout` when the "+
			"plugin is enabled, 30s by default), and a re-sent call carries nothing to serve. "+
			"The work the first call started is unaffected and this one cannot be answered. "+
			"Note the direction of that lever: this plugin is never told the value you enabled "+
			"it with, so it sizes its own work to the 30s default and cannot follow yours. "+
			"A `--timeout` BELOW 30s makes calls fail here every time and is not supported; "+
			"above it, the extra time is never used. If the exchange itself is slow, the "+
			"server or the segment is what to change: %w", err)
	}
	return fmt.Errorf("failed to parse request body: %w", err)
}
