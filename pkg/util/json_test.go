package util

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJSONResponse_OK(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONResponse(rec, map[string]string{"hello": "world"}, http.StatusCreated)

	if got := rec.Code; got != http.StatusCreated {
		t.Fatalf("status: got %d want %d", got, http.StatusCreated)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: got %q want application/json", ct)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if out["hello"] != "world" {
		t.Fatalf("body: got %v", out)
	}
}

// unencodable forces json.Encode to fail so we can exercise the 500 fallback.
type unencodable struct{}

func (unencodable) MarshalJSON() ([]byte, error) { return nil, errors.New("nope") }

func TestJSONResponse_EncodeFailureFallsBackTo500(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONResponse(rec, unencodable{}, http.StatusOK)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type: got %q want text/plain*", ct)
	}
	if !strings.Contains(rec.Body.String(), "Failed to serialize") {
		t.Fatalf("body: got %q", rec.Body.String())
	}
}

func TestJSONErrResponse_UsesErrToStatusWhenZero(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONErrResponse(rec, ErrParentRequired, 0)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type: got %q", ct)
	}
	var out struct {
		Err string `json:"Err"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Err != ErrParentRequired.Error() {
		t.Fatalf("err body: got %q", out.Err)
	}
}

func TestJSONErrResponse_ExplicitStatusOverrides(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONErrResponse(rec, errors.New("anything"), http.StatusTeapot)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status: got %d want 418", rec.Code)
	}
}

func TestParseJSONOrErrorResponse_OK(t *testing.T) {
	body := strings.NewReader(`{"name":"foo"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()

	var v struct {
		Name string `json:"name"`
	}
	if err := ParseJSONOrErrorResponse(&v, rec, req); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if v.Name != "foo" {
		t.Fatalf("decoded: got %+v", v)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("recorder default code changed unexpectedly: %d", rec.Code)
	}
}

func TestParseJSONOrErrorResponse_BadJSONWrites400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()

	var v struct{}
	if err := ParseJSONOrErrorResponse(&v, rec, req); err == nil {
		t.Fatal("expected error from invalid JSON")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestParseJSONOrErrorResponse_UnknownFieldRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"unexpected":1}`))
	rec := httptest.NewRecorder()

	var v struct {
		Known string `json:"known"`
	}
	if err := ParseJSONOrErrorResponse(&v, rec, req); err == nil {
		t.Fatal("expected error from unknown field")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestAwaitCondition_OkImmediately(t *testing.T) {
	calls := 0
	err := AwaitCondition(context.Background(), func() (bool, error) {
		calls++
		return true, nil
	}, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls: got %d want 1", calls)
	}
}

func TestAwaitCondition_OkAfterPolling(t *testing.T) {
	calls := 0
	err := AwaitCondition(context.Background(), func() (bool, error) {
		calls++
		return calls >= 3, nil
	}, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if calls < 3 {
		t.Fatalf("calls: got %d want >=3", calls)
	}
}

func TestAwaitCondition_PropagatesCondError(t *testing.T) {
	want := errors.New("boom")
	err := AwaitCondition(context.Background(), func() (bool, error) {
		return false, want
	}, time.Millisecond)
	if !errors.Is(err, want) {
		t.Fatalf("err: got %v want %v", err, want)
	}
}

func TestAwaitCondition_RespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := AwaitCondition(ctx, func() (bool, error) {
		return false, nil
	}, 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err: got %v want DeadlineExceeded", err)
	}
}
