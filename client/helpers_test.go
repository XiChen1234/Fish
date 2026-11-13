package client_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestSrv(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}
