package docker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	registrydocker "github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/mbianchidev/porto/internal/registries"
)

const maxRegistrySearchResults = 100

func registryHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: httpsRegistryTransport{http.DefaultTransport},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" || len(via) >= 5 {
				return errors.New("registry redirect must use HTTPS and stay within five redirects")
			}
			return nil
		},
	}
}

type httpsRegistryTransport struct {
	http.RoundTripper
}

func (transport httpsRegistryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.User != nil {
		return nil, errors.New("registry authentication requires HTTPS without embedded URL credentials")
	}
	return transport.RoundTripper.RoundTrip(request)
}

func (a *API) registryLogin(w http.ResponseWriter, r *http.Request) {
	var auth RegistryAuth
	if !decodeDockerOptionsJSON(w, r, &auth) {
		return
	}
	server := auth.ServerAddress
	if server == "" {
		server = dockerHubRegistry
	}
	server, err := registries.NormalizeServer(server)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	host := server
	if server == dockerHubRegistry {
		host = "registry-1.docker.io"
	}
	username, password := auth.Username, auth.Password
	if auth.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
		if err != nil {
			writeDockerError(w, errors.New("invalid registry basic authentication"))
			return
		}
		var valid bool
		username, password, valid = strings.Cut(string(decoded), ":")
		if !valid {
			writeDockerError(w, errors.New("invalid registry basic authentication"))
			return
		}
	}
	if auth.IdentityToken != "" {
		username, password = "", auth.IdentityToken
	}
	client := a.registryClient
	authorizer := registrydocker.NewDockerAuthorizer(
		registrydocker.WithAuthClient(client),
		registrydocker.WithAuthCreds(func(requestHost string) (string, string, error) {
			if requestHost != host {
				return "", "", errors.New("registry credentials requested for an unexpected host")
			}
			return username, password, nil
		}),
	)
	for attempt := 0; attempt < 3; attempt++ {
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://"+host+"/v2/", nil)
		if err != nil {
			writeDockerError(w, err)
			return
		}
		if auth.RegistryToken != "" {
			request.Header.Set("Authorization", "Bearer "+auth.RegistryToken)
		} else if err := authorizer.Authorize(r.Context(), request); err != nil {
			writeDockerJSON(w, http.StatusUnauthorized, map[string]string{"message": "registry authentication failed"})
			return
		}
		response, err := client.Do(request)
		if err != nil {
			writeDockerError(w, fmt.Errorf("contact registry: %w", err))
			return
		}
		_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			writeDockerError(w, fmt.Errorf("read registry authentication response: %w", errors.Join(readErr, closeErr)))
			return
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			writeDockerJSON(w, http.StatusOK, map[string]string{"Status": "Login Succeeded", "IdentityToken": auth.IdentityToken})
			return
		}
		if response.StatusCode != http.StatusUnauthorized {
			writeDockerError(w, fmt.Errorf("registry authentication returned HTTP %d", response.StatusCode))
			return
		}
		if auth.RegistryToken != "" || authorizer.AddResponses(r.Context(), []*http.Response{response}) != nil {
			break
		}
	}
	writeDockerJSON(w, http.StatusUnauthorized, map[string]string{"message": "registry authentication failed"})
}

func (a *API) registrySearch(w http.ResponseWriter, r *http.Request) {
	term := strings.TrimSpace(r.URL.Query().Get("term"))
	if term == "" || strings.ContainsAny(term, "\r\n\x00") {
		writeDockerError(w, errors.New("registry search term is required"))
		return
	}
	limit := 25
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxRegistrySearchResults {
			writeDockerError(w, errors.New("registry search limit must be between 1 and 100"))
			return
		}
	}
	filters, err := parseDockerFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	stars := 0
	for key, values := range filters {
		switch key {
		case "stars":
			for value, enabled := range values {
				if !enabled {
					continue
				}
				minimum, err := strconv.Atoi(value)
				if err != nil || minimum < 0 {
					writeDockerError(w, errors.New("invalid registry star filter"))
					return
				}
				stars = max(stars, minimum)
			}
		case "is-official", "is-automated":
			for value := range values {
				if value != "true" && value != "false" {
					writeDockerError(w, errors.New("invalid registry boolean filter"))
					return
				}
			}
		default:
			writeDockerUnsupported(w, "registry search filter "+key)
			return
		}
	}
	host := "index.docker.io"
	if first, rest, ok := strings.Cut(term, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		server, err := registries.NormalizeServer(first)
		if err != nil {
			writeDockerError(w, err)
			return
		}
		host, term = server, rest
		if server == dockerHubRegistry {
			host = "index.docker.io"
		}
	}
	endpoint := "https://" + host + "/v1/search?" + url.Values{
		"q": {term}, "n": {strconv.Itoa(limit)},
	}.Encode()
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	response, err := a.registryClient.Do(request)
	if err != nil {
		writeDockerError(w, fmt.Errorf("search registry: %w", err))
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		writeDockerError(w, fmt.Errorf("registry search returned HTTP %d", response.StatusCode))
		return
	}
	type searchResult struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		StarCount   int    `json:"star_count"`
		IsOfficial  bool   `json:"is_official"`
		IsAutomated bool   `json:"is_automated"`
	}
	var document struct {
		Results []searchResult `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4*1024*1024)).Decode(&document); err != nil {
		writeDockerError(w, fmt.Errorf("decode registry search: %w", err))
		return
	}
	result := make([]searchResult, 0, maxRegistrySearchResults)
	for _, item := range document.Results {
		if item.StarCount < stars ||
			!filters.matchesValue("is-official", strconv.FormatBool(item.IsOfficial)) ||
			!filters.matchesValue("is-automated", strconv.FormatBool(item.IsAutomated)) {
			continue
		}
		result = append(result, item)
		if len(result) == limit {
			break
		}
	}
	writeDockerJSON(w, http.StatusOK, result)
}
