package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	statusRoute = "/v0/management/" + pluginID + "/status"
	pageCSP     = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"
)

//go:embed status.html
var statusPage []byte

type managementRoute struct {
	Method      string `json:"Method,omitempty"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementRegistration struct {
	Routes    []managementRoute `json:"routes"`
	Resources []managementRoute `json:"resources"`
}

func managementRoutes() managementRegistration {
	return managementRegistration{
		Routes: []managementRoute{{Method: http.MethodGet, Path: statusRoute}},
		Resources: []managementRoute{{
			Path:        "status",
			Menu:        "Reset First",
			Description: "Quota usage, resets, and routing order for each subscription.",
		}},
	}
}

func handleManagement(req pluginapi.ManagementRequest, r *router) pluginapi.ManagementResponse {
	if !strings.HasSuffix(req.Path, "/status") {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound}
	}

	if req.Method != http.MethodGet {
		return pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: http.Header{"Allow": {http.MethodGet}}}
	}

	switch {
	case strings.HasPrefix(req.Path, "/v0/management/"):
		body, err := json.Marshal(r.Status())
		if err != nil {
			return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError}
		}

		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Cache-Control": {"no-store"},
				"Content-Type":  {"application/json; charset=utf-8"},
			},
			Body: body,
		}

	case strings.HasPrefix(req.Path, "/v0/resource/plugins/"):
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Cache-Control":           {"no-store"},
				"Content-Security-Policy": {pageCSP},
				"Content-Type":            {"text/html; charset=utf-8"},
				"Referrer-Policy":         {"no-referrer"},
				"X-Content-Type-Options":  {"nosniff"},
			},
			Body: statusPage,
		}

	default:
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound}
	}
}
