package internal

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/traefik/genconf/dynamic"
)

func catchError(args ...any) error {
	if ln := len(args); ln < 0 {
		return nil
	} else if err, ok := args[ln-1].(error); ok {
		return err
	}

	return nil
}

func TestClient_Endpoint(t *testing.T) {
	var client *Client
	require.NotPanics(t, func() {
		require.Equal(t, client.Endpoint(), "empty")
	})
}

func TestClient_FetchErrors(t *testing.T) {
	cli := new(Client)
	cli.Client = new(http.Client)
	{
		out := make(chan *dynamic.Configuration, 2)
		require.ErrorContains(t, cli.FetchRaw(nil, out), "nil Context") // nolint:staticcheck
		close(out)
		require.Len(t, out, 1)
		require.Nil(t, <-out)
	}

	{
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		out := make(chan *dynamic.Configuration, 2)
		require.ErrorIs(t, cli.FetchRaw(ctx, out), context.Canceled)
		close(out)
		require.Len(t, out, 1)
		require.Nil(t, <-out)
	}
}

func testHandler(t *testing.T, rawdata []byte, entrypoints string) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/rawdata":
			w.WriteHeader(http.StatusOK)
			assert.NoError(t, catchError(w.Write(rawdata)))
		case "/api/entrypoints":
			w.WriteHeader(http.StatusOK)
			assert.NoError(t, catchError(w.Write([]byte(entrypoints))))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

func entryPointsJSON(port int, names ...string) string {
	result := "["
	for i, name := range names {
		if i > 0 {
			result += ","
		}
		result += fmt.Sprintf(`{"name":%q,"address":":%d/tcp"}`, name, port)
	}
	result += "]"
	return result
}

func TestClient(t *testing.T) {
	data, err := os.ReadFile("../fixtures/jaeger-api-rawdata.json")
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)

		assert.NoError(t, catchError(w.Write(data)))
	}))

	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond*100)
	defer cancel()

	resolver := "letsencrypt"

	cfg := Config{
		ConnTimeout:  defaultTestConnTimeout,
		PollInterval: defaultTestPollInterval,
		TLSResolver:  &resolver,
		Endpoints: []Endpoint{{
			Host: addr.IP.String(),
			API:  addr.Port,
			WEB:  addr.Port,
		}},
	}

	cli, err := cfg.PrepareClients(ctx)
	require.NoError(t, err)

	require.Equal(t, cli[0].Endpoint(), addr.IP.String())

	out := make(chan *dynamic.Configuration, 1)
	if err = cli[0].FetchRaw(t.Context(), out); err != nil {
		t.Fatal(err)
	}

	var result *dynamic.Configuration
	select {
	case <-ctx.Done():
		t.Fatal("no response")
	case result = <-out:
		require.NotEmpty(t, result)
		require.Equal(t, &dynamic.Configuration{
			HTTP: &dynamic.HTTPConfiguration{
				Routers: map[string]*dynamic.Router{
					"web-whoami-" + addr.IP.String(): {
						Middlewares: []string{"http2https"},
						Service:     "web-whoami-" + addr.IP.String(),
						Rule:        "Host(`whoami.example.com`)",
					},
					"web-whoami-" + addr.IP.String() + "-secure": {
						Service: "web-whoami-" + addr.IP.String(),
						Rule:    "Host(`whoami.example.com`)",
						TLS:     &dynamic.RouterTLSConfig{CertResolver: resolver},
					},
					"websecure-whoami-" + addr.IP.String(): {
						Middlewares: []string{"http2https"},
						Service:     "websecure-whoami-" + addr.IP.String(),
						Rule:        "Host(`whoami.example.com`)",
					},
					"websecure-whoami-" + addr.IP.String() + "-secure": {
						Service: "websecure-whoami-" + addr.IP.String(),
						Rule:    "Host(`whoami.example.com`)",
						TLS:     &dynamic.RouterTLSConfig{CertResolver: resolver},
					},
					"errors-" + addr.IP.String(): {
						Middlewares: []string{"http2https"},
						Service:     "errors-" + addr.IP.String(),
						Rule:        "HostRegexp(`.*`)",
					},
					"errors-" + addr.IP.String() + "-secure": {
						Service: "errors-" + addr.IP.String(),
						Rule:    "HostRegexp(`.*`)",
						TLS:     &dynamic.RouterTLSConfig{CertResolver: resolver},
					},
				},
				Services: map[string]*dynamic.Service{
					"web-whoami-" + addr.IP.String(): {
						LoadBalancer: &dynamic.ServersLoadBalancer{
							Servers: []dynamic.Server{{URL: (&url.URL{
								Scheme: "http",
								Host:   addr.String(),
								Path:   defaultPath,
							}).String()}},
						},
					},
					"websecure-whoami-" + addr.IP.String(): {
						LoadBalancer: &dynamic.ServersLoadBalancer{
							Servers: []dynamic.Server{{URL: (&url.URL{
								Scheme: "http",
								Host:   addr.String(),
								Path:   defaultPath,
							}).String()}},
						},
					},
					"errors-" + addr.IP.String(): {
						LoadBalancer: &dynamic.ServersLoadBalancer{
							Servers: []dynamic.Server{{URL: (&url.URL{
								Scheme: "http",
								Host:   addr.String(),
								Path:   defaultPath,
							}).String()}},
						},
					},
				},
				Middlewares: map[string]*dynamic.Middleware{
					"http2https": {RedirectScheme: &dynamic.RedirectScheme{
						Scheme:    "https",
						Permanent: true,
					}},
				},
			},
		}, result)
	}
}

func TestClient_tls(t *testing.T) {
	data, err := os.ReadFile("../fixtures/jaeger-api-rawdata.json")
	require.NoError(t, err)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)

		assert.NoError(t, catchError(w.Write(data)))
	}))

	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond*100)
	defer cancel()

	resolver := "letsencrypt"

	cfg := Config{
		ConnTimeout:  defaultTestConnTimeout,
		PollInterval: defaultTestPollInterval,
		TLSResolver:  &resolver,
		Endpoints: []Endpoint{{
			Host: addr.IP.String(),
			API:  addr.Port,
			WEB:  addr.Port,
			TLS:  &TLS{IgnoreInsecure: true},
		}},
	}

	cli, err := cfg.PrepareClients(ctx)
	require.NoError(t, err)

	require.Equal(t, cli[0].Endpoint(), addr.IP.String())

	out := make(chan *dynamic.Configuration, 1)
	if err = cli[0].FetchRaw(t.Context(), out); err != nil {
		t.Fatal(err)
	}

	var result *dynamic.Configuration
	select {
	case <-ctx.Done():
		t.Fatal("no response")
	case result = <-out:
		require.NotEmpty(t, result)
		require.Equal(t, &dynamic.Configuration{
			HTTP: &dynamic.HTTPConfiguration{
				Routers: map[string]*dynamic.Router{
					"web-whoami-" + addr.IP.String(): {
						Middlewares: []string{"http2https"},
						Service:     "web-whoami-" + addr.IP.String(),
						Rule:        "Host(`whoami.example.com`)",
					},
					"web-whoami-" + addr.IP.String() + "-secure": {
						Service: "web-whoami-" + addr.IP.String(),
						Rule:    "Host(`whoami.example.com`)",
						TLS:     &dynamic.RouterTLSConfig{CertResolver: resolver},
					},
					"websecure-whoami-" + addr.IP.String(): {
						Middlewares: []string{"http2https"},
						Service:     "websecure-whoami-" + addr.IP.String(),
						Rule:        "Host(`whoami.example.com`)",
					},
					"websecure-whoami-" + addr.IP.String() + "-secure": {
						Service: "websecure-whoami-" + addr.IP.String(),
						Rule:    "Host(`whoami.example.com`)",
						TLS:     &dynamic.RouterTLSConfig{CertResolver: resolver},
					},
					"errors-" + addr.IP.String(): {
						Middlewares: []string{"http2https"},
						Service:     "errors-" + addr.IP.String(),
						Rule:        "HostRegexp(`.*`)",
					},
					"errors-" + addr.IP.String() + "-secure": {
						Service: "errors-" + addr.IP.String(),
						Rule:    "HostRegexp(`.*`)",
						TLS:     &dynamic.RouterTLSConfig{CertResolver: resolver},
					},
				},
				Services: map[string]*dynamic.Service{
					"web-whoami-" + addr.IP.String(): {
						LoadBalancer: &dynamic.ServersLoadBalancer{
							Servers: []dynamic.Server{{URL: (&url.URL{
								Scheme: "https",
								Host:   addr.String(),
								Path:   defaultPath,
							}).String()}},
						},
					},
					"websecure-whoami-" + addr.IP.String(): {
						LoadBalancer: &dynamic.ServersLoadBalancer{
							Servers: []dynamic.Server{{URL: (&url.URL{
								Scheme: "https",
								Host:   addr.String(),
								Path:   defaultPath,
							}).String()}},
						},
					},
					"errors-" + addr.IP.String(): {
						LoadBalancer: &dynamic.ServersLoadBalancer{
							Servers: []dynamic.Server{{URL: (&url.URL{
								Scheme: "https",
								Host:   addr.String(),
								Path:   defaultPath,
							}).String()}},
						},
					},
				},
				Middlewares: map[string]*dynamic.Middleware{
					"http2https": {RedirectScheme: &dynamic.RedirectScheme{
						Scheme:    "https",
						Permanent: true,
					}},
				},
			},
		}, result)
	}
}

func TestClient_empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)

		assert.NoError(t, catchError(w.Write([]byte(`{}`))))
	}))

	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond*100)
	defer cancel()

	resolver := "letsencrypt"

	cfg := Config{
		ConnTimeout:  defaultTestConnTimeout,
		PollInterval: defaultTestPollInterval,
		TLSResolver:  &resolver,
		Endpoints: []Endpoint{{
			Host: addr.IP.String(),
			API:  addr.Port,
			WEB:  addr.Port,
		}},
	}

	cli, err := cfg.PrepareClients(ctx)
	require.NoError(t, err)

	out := make(chan *dynamic.Configuration, 1)
	require.ErrorIs(t, cli[0].FetchRaw(t.Context(), out), ErrEmptyResponse)

	select {
	case <-ctx.Done():
		t.Fatal("expect result")
	case msg := <-out:
		require.Empty(t, msg)
	}
}

const noServiceResponse = `{"services": {"two@docker": {}}, "routers": {"one@docker": {}}}`

func TestClient_noService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)

		assert.NoError(t, catchError(w.Write([]byte(noServiceResponse))))
	}))

	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond*100)
	defer cancel()

	resolver := "letsencrypt"

	cfg := Config{
		ConnTimeout:  defaultTestConnTimeout,
		PollInterval: defaultTestPollInterval,
		TLSResolver:  &resolver,
		Endpoints: []Endpoint{{
			Host: addr.IP.String(),
			API:  addr.Port,
			WEB:  addr.Port,
		}},
	}

	cli, err := cfg.PrepareClients(ctx)
	require.NoError(t, err)

	out := make(chan *dynamic.Configuration, 1)
	require.NoError(t, cli[0].FetchRaw(t.Context(), out))

	select {
	case <-ctx.Done():
		t.Fatal("expect result")
	case msg := <-out:
		require.Empty(t, msg)
	}
}

func TestClient_failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))

	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond*100)
	defer cancel()

	resolver := "letsencrypt"

	cfg := Config{
		ConnTimeout:  defaultTestConnTimeout,
		PollInterval: defaultTestPollInterval,
		TLSResolver:  &resolver,
		Endpoints: []Endpoint{{
			Host: addr.IP.String(),
			API:  addr.Port,
			WEB:  addr.Port,
		}},
	}

	cli, err := cfg.PrepareClients(ctx)
	require.NoError(t, err)

	out := make(chan *dynamic.Configuration, 1)
	require.ErrorIs(t, cli[0].FetchRaw(t.Context(), out), io.EOF)

	select {
	case <-ctx.Done():
		t.Fatal("expect result")
	case msg := <-out:
		require.Empty(t, msg)
	}
}

func TestClient_entryPointFilter_inferred(t *testing.T) {
	data, err := os.ReadFile("../fixtures/jaeger-api-rawdata.json")
	require.NoError(t, err)

	// entrypoints: only "web" matches the server port, "websecure" is on 443
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/entrypoints":
			addr := r.Host
			if _, p, err := net.SplitHostPort(addr); err == nil {
				w.WriteHeader(http.StatusOK)
				resp := fmt.Sprintf(`[{"name":"web","address":":%s/tcp"},{"name":"websecure","address":":443/tcp"}]`, p)
				assert.NoError(t, catchError(w.Write([]byte(resp))))
			}
		case "/api/rawdata":
			w.WriteHeader(http.StatusOK)
			assert.NoError(t, catchError(w.Write(data)))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))

	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond*200)
	defer cancel()

	resolver := "letsencrypt"

	cfg := Config{
		ConnTimeout:  defaultTestConnTimeout,
		PollInterval: defaultTestPollInterval,
		TLSResolver:  &resolver,
		Endpoints: []Endpoint{{
			Host: addr.IP.String(),
			API:  addr.Port,
			WEB:  addr.Port,
		}},
	}

	cli, err := cfg.PrepareClients(ctx)
	require.NoError(t, err)

	// only "web" entrypoint matches, so websecure-whoami should be filtered out
	require.Equal(t, []string{"web"}, cli[0].entryPointNames)

	out := make(chan *dynamic.Configuration, 1)
	require.NoError(t, cli[0].FetchRaw(t.Context(), out))

	result := <-out
	require.NotNil(t, result)
	require.NotNil(t, result.HTTP)

	// web-whoami (entrypoint: web) should be present
	assert.Contains(t, result.HTTP.Routers, "web-whoami-"+addr.IP.String())
	assert.Contains(t, result.HTTP.Services, "web-whoami-"+addr.IP.String())

	// errors (entrypoint: web) should be present
	assert.Contains(t, result.HTTP.Routers, "errors-"+addr.IP.String())
	assert.Contains(t, result.HTTP.Services, "errors-"+addr.IP.String())

	// websecure-whoami (entrypoint: websecure) should be filtered out
	assert.NotContains(t, result.HTTP.Routers, "websecure-whoami-"+addr.IP.String())
	assert.NotContains(t, result.HTTP.Services, "websecure-whoami-"+addr.IP.String())
}

func TestClient_entryPointFilter_explicit(t *testing.T) {
	data, err := os.ReadFile("../fixtures/jaeger-api-rawdata.json")
	require.NoError(t, err)

	// server doesn't need to serve entrypoints API when explicit config is used
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, catchError(w.Write(data)))
	}))

	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond*200)
	defer cancel()

	resolver := "letsencrypt"

	cfg := Config{
		ConnTimeout:  defaultTestConnTimeout,
		PollInterval: defaultTestPollInterval,
		TLSResolver:  &resolver,
		Endpoints: []Endpoint{{
			Host:       addr.IP.String(),
			API:        addr.Port,
			WEB:        addr.Port,
			EntryPoint: "websecure",
		}},
	}

	cli, err := cfg.PrepareClients(ctx)
	require.NoError(t, err)

	require.Equal(t, []string{"websecure"}, cli[0].entryPointNames)

	out := make(chan *dynamic.Configuration, 1)
	require.NoError(t, cli[0].FetchRaw(t.Context(), out))

	result := <-out
	require.NotNil(t, result)
	require.NotNil(t, result.HTTP)

	// websecure-whoami (entrypoint: websecure) should be present
	assert.Contains(t, result.HTTP.Routers, "websecure-whoami-"+addr.IP.String())
	assert.Contains(t, result.HTTP.Services, "websecure-whoami-"+addr.IP.String())

	// web-whoami (entrypoint: web) should be filtered out
	assert.NotContains(t, result.HTTP.Routers, "web-whoami-"+addr.IP.String())
	assert.NotContains(t, result.HTTP.Services, "web-whoami-"+addr.IP.String())

	// errors (entrypoint: web) should be filtered out
	assert.NotContains(t, result.HTTP.Routers, "errors-"+addr.IP.String())
	assert.NotContains(t, result.HTTP.Services, "errors-"+addr.IP.String())
}

func TestClient_entryPointFilter_multipleMatch(t *testing.T) {
	data, err := os.ReadFile("../fixtures/jaeger-api-rawdata.json")
	require.NoError(t, err)

	// both web and websecure on the same port
	srv := httptest.NewServer(testHandler(t, data, ""))

	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	// recreate server on same address with correct entrypoints JSON
	srv.Close()
	epJSON := entryPointsJSON(addr.Port, "web", "websecure")
	srv = httptest.NewUnstartedServer(testHandler(t, data, epJSON))
	srv.Listener, _ = net.Listen("tcp", addr.String())
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond*200)
	defer cancel()

	resolver := "letsencrypt"

	cfg := Config{
		ConnTimeout:  defaultTestConnTimeout,
		PollInterval: defaultTestPollInterval,
		TLSResolver:  &resolver,
		Endpoints: []Endpoint{{
			Host: addr.IP.String(),
			API:  addr.Port,
			WEB:  addr.Port,
		}},
	}

	cli, err := cfg.PrepareClients(ctx)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"web", "websecure"}, cli[0].entryPointNames)

	out := make(chan *dynamic.Configuration, 1)
	require.NoError(t, cli[0].FetchRaw(t.Context(), out))

	result := <-out
	require.NotNil(t, result)
	require.NotNil(t, result.HTTP)

	// all routers should be present since both entrypoints match
	assert.Contains(t, result.HTTP.Routers, "web-whoami-"+addr.IP.String())
	assert.Contains(t, result.HTTP.Routers, "websecure-whoami-"+addr.IP.String())
	assert.Contains(t, result.HTTP.Routers, "errors-"+addr.IP.String())
}
