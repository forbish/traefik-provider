package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/traefik/genconf/dynamic"
)

type Client struct {
	*http.Client

	endpoint        Endpoint
	resolver        *string
	entryPointNames []string
}

const defaultRawPath = "/api/rawdata"
const entryPointsPath = "/api/entrypoints"

var ErrEmptyResponse = errors.New("received empty response")

func (c *Client) Endpoint() string {
	if c == nil {
		return "empty"
	}

	return c.endpoint.Host
}

func (c *Client) httpCall(ctx context.Context) (*dynamic.Configuration, error) {
	uri := c.endpoint.buildURI(c.endpoint.API, defaultRawPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("could not prepare request for %s: %w", uri, err)
	}

	var res *http.Response
	if res, err = c.Do(req); err != nil {
		return nil, fmt.Errorf("could not make request for %s: %w", uri, err)
	}

	buf := new(bytes.Buffer)
	tee := io.TeeReader(res.Body, buf)

	var result dynamic.Configuration
	if err = json.NewDecoder(tee).Decode(&result.HTTP); err != nil {
		return nil, fmt.Errorf("could not decode response for %s: %s: %w", uri, buf.String(), err)
	}

	return &result, res.Body.Close()
}

func (c *Client) prepareResponse(res *dynamic.Configuration) *dynamic.Configuration {
	var output dynamic.Configuration
	for key, item := range res.HTTP.Routers {
		if strings.HasSuffix(key, "@internal") {
			continue
		}

		if len(c.entryPointNames) > 0 && !c.routerMatchesEntryPoint(item) {
			continue
		}

		parts := strings.Split(key, "@")
		name := fmt.Sprintf("%s-%s", parts[0], c.endpoint.Host)
		routerSvc := item.Service

		var service *dynamic.Service
		if svc, exists := res.HTTP.Services[key]; exists {
			service = svc
		}
		if service == nil {
			if svc, exists := res.HTTP.Services[routerSvc]; exists {
				service = svc
			}
		}
		if service == nil && len(parts) > 1 {
			qualifiedSvc := routerSvc + "@" + parts[1]
			if svc, exists := res.HTTP.Services[qualifiedSvc]; exists {
				service = svc
			}
		}
		if service == nil || service.LoadBalancer == nil {
			continue
		}

		if output.HTTP == nil {
			output.HTTP = &dynamic.HTTPConfiguration{
				Routers:     make(map[string]*dynamic.Router),
				Services:    make(map[string]*dynamic.Service),
				Middlewares: make(map[string]*dynamic.Middleware),
			}
		}

		output.HTTP.Routers[name] = &dynamic.Router{
			Service: name,
			Rule:    item.Rule,
		}

		var servers []dynamic.Server
		for range service.LoadBalancer.Servers {
			servers = append(servers, dynamic.Server{
				URL: c.endpoint.buildURI(c.endpoint.WEB, defaultPath),
			})
		}

		output.HTTP.Services[name] = &dynamic.Service{
			LoadBalancer: &dynamic.ServersLoadBalancer{Servers: servers},
		}

		if c.resolver != nil {
			output.HTTP.Routers[name].Middlewares = append(
				output.HTTP.Routers[name].Middlewares,
				"http2https",
			)

			output.HTTP.Routers[name+"-secure"] = &dynamic.Router{
				Service: name,
				Rule:    item.Rule,
				TLS:     &dynamic.RouterTLSConfig{CertResolver: *c.resolver},
			}

			output.HTTP.Middlewares["http2https"] = &dynamic.Middleware{
				RedirectScheme: &dynamic.RedirectScheme{Scheme: "https", Permanent: true},
			}
		}
	}

	return &output
}

func (c *Client) FetchRaw(ctx context.Context, out chan<- *dynamic.Configuration) error {
	if res, err := c.httpCall(ctx); err != nil {
		out <- nil

		return err
	} else if len(res.HTTP.Routers) > 0 && len(res.HTTP.Services) > 0 {
		out <- c.prepareResponse(res)

		return nil
	}

	out <- nil

	return fmt.Errorf("%w (1client:%q)", ErrEmptyResponse, c.endpoint.Host)
}

func (c *Client) routerMatchesEntryPoint(router *dynamic.Router) bool {
	for _, ep := range router.EntryPoints {
		for _, allowed := range c.entryPointNames {
			if ep == allowed {
				return true
			}
		}
	}

	return false
}

type apiEntryPoint struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

func (c *Client) resolveEntryPoints(ctx context.Context) []string {
	if c.endpoint.EntryPoint != "" {
		return []string{c.endpoint.EntryPoint}
	}

	uri := c.endpoint.buildURI(c.endpoint.API, entryPointsPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		log.Printf("could not resolve entrypoints for %s, all routers will be included: %v", c.endpoint.Host, err)
		return nil
	}

	res, err := c.Do(req)
	if err != nil {
		log.Printf("could not resolve entrypoints for %s, all routers will be included: %v", c.endpoint.Host, err)
		return nil
	}
	defer func() { _ = res.Body.Close() }()

	var entryPoints []apiEntryPoint
	if err = json.NewDecoder(res.Body).Decode(&entryPoints); err != nil {
		log.Printf("could not decode entrypoints for %s, all routers will be included: %v", c.endpoint.Host, err)
		return nil
	}

	target := strconv.Itoa(c.endpoint.WEB)
	var matches []string
	for _, ep := range entryPoints {
		// address format is ":port" or ":port/protocol"
		addr := ep.Address
		if idx := strings.Index(addr, "/"); idx != -1 {
			addr = addr[:idx]
		}
		port := strings.TrimPrefix(addr, ":")
		if port == target {
			matches = append(matches, ep.Name)
		}
	}

	if len(matches) == 0 {
		log.Printf("no entrypoints found matching webPort %d on %s, all routers will be included", c.endpoint.WEB, c.endpoint.Host)
	}

	return matches
}
