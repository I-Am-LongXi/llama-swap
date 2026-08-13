package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func fallbackTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  a:
    cmd: echo ${PORT}
    aliases: [alias-a]
  b:
    cmd: echo ${PORT}
peers:
  remote:
    proxy: http://example.com
    models: [remote-model]
`))
	require.NoError(t, err)
	return cfg
}

// fallbackTestServer wires a Server whose local router reports the given
// running models, and captures what the dispatch handler finally receives.
func fallbackTestServer(t *testing.T, running map[string]process.ProcessState) (*Server, *swaputil.ReqContextData, *[]byte) {
	t.Helper()
	local := newStubRouter([]string{"a", "b"}, "")
	local.running = running
	peer := newStubRouter([]string{"remote/remote-model"}, "")

	var received swaputil.ReqContextData
	var body []byte
	capture := func(w http.ResponseWriter, r *http.Request) {
		received, _ = swaputil.ReadContext(r.Context())
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{}}`))
	}
	local.serveHTTP = capture
	peer.serveHTTP = capture

	s := newTestServer(local, peer)
	s.cfg = fallbackTestConfig(t)
	s.routes()
	t.Cleanup(func() { s.store.Close() })

	return s, &received, &body
}

func TestServer_FallbackMiddleware_UnknownModelRoutesToActive(t *testing.T) {
	s, received, body := fallbackTestServer(t, map[string]process.ProcessState{
		"b": process.StateReady,
	})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("totally-bogus"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, "b", received.ModelID)
	// The upstream request body must carry the fallback model, not the
	// bogus name the client sent.
	assert.Equal(t, "b", gjson.GetBytes(*body, "model").String())
}

func TestServer_FallbackMiddleware_KnownModelUntouched(t *testing.T) {
	for _, name := range []string{"a", "alias-a"} {
		t.Run(name, func(t *testing.T) {
			s, received, body := fallbackTestServer(t, map[string]process.ProcessState{
				"b": process.StateReady,
			})

			w := httptest.NewRecorder()
			s.ServeHTTP(w, chatRequest(name))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			assert.Equal(t, "a", received.ModelID)
			assert.Equal(t, name, gjson.GetBytes(*body, "model").String())
		})
	}
}

func TestServer_FallbackMiddleware_PeerModelUntouched(t *testing.T) {
	s, received, body := fallbackTestServer(t, map[string]process.ProcessState{
		"b": process.StateReady,
	})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("remote/remote-model"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// A peer model is a real target: it must reach the peer router untouched
	// rather than being redirected to the locally running "b".
	assert.Equal(t, "remote/remote-model", received.ModelID)
	assert.Equal(t, "remote/remote-model", gjson.GetBytes(*body, "model").String())
}

func TestServer_FallbackMiddleware_NoRunningModels(t *testing.T) {
	s, _, _ := fallbackTestServer(t, nil)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("totally-bogus"))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestServer_FallbackMiddleware_StoppingModelsAreNotTargets(t *testing.T) {
	s, _, _ := fallbackTestServer(t, map[string]process.ProcessState{
		"a": process.StateStopping,
		"b": process.StateShutdown,
	})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("totally-bogus"))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestServer_FallbackMiddleware_PrefersReadyOverStarting(t *testing.T) {
	// "a" sorts first but is still loading, so the ready "b" must win.
	s, received, _ := fallbackTestServer(t, map[string]process.ProcessState{
		"a": process.StateStarting,
		"b": process.StateReady,
	})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("totally-bogus"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "b", received.ModelID)
}

func TestServer_ActiveModel(t *testing.T) {
	tests := []struct {
		name    string
		running map[string]process.ProcessState
		want    string
		found   bool
	}{
		{"nothing running", nil, "", false},
		{
			"ready wins over starting",
			map[string]process.ProcessState{"a": process.StateStarting, "z": process.StateReady},
			"z", true,
		},
		{
			"starting used when nothing is ready",
			map[string]process.ProcessState{"z": process.StateStarting, "a": process.StateStarting},
			"a", true,
		},
		{
			"sorted tiebreak between ready models",
			map[string]process.ProcessState{"c": process.StateReady, "b": process.StateReady},
			"b", true,
		},
		{
			"stopping and shutdown are ignored",
			map[string]process.ProcessState{"a": process.StateStopping, "b": process.StateShutdown},
			"", false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, found := activeModel(tc.running)
			assert.Equal(t, tc.found, found)
			assert.Equal(t, tc.want, got)
		})
	}
}
