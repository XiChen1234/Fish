package msg_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

var testServerURL string

func newHTTPTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	testServerURL = ts.URL
	t.Cleanup(ts.Close)
	return ts
}

func serverURL() string { return testServerURL }

func trimHTTP(u string) string { return strings.TrimPrefix(u, "http") }

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// dialClientRaw 建立 WS 连接但不读取 ready 帧（调用方自行处理首帧）。
func dialClientRaw(t *testing.T, tsURL, token string) *websocket.Conn {
	t.Helper()
	u := "ws" + trimHTTP(tsURL) + "/ws?token=" + token
	ws, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("连接失败 token=%s: %v (resp=%v)", token, err, resp)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}
