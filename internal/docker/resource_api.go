package docker

import "net/http"

func (a *API) networks(w http.ResponseWriter, r *http.Request) {
	filters, err := parseDockerFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	networks, err := a.manager.Networks(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}
	response := make([]map[string]any, 0, len(networks))
	for _, network := range networks {
		if !filters.matchesID(network.ID) ||
			!filters.matchesName(network.Name) ||
			!filters.matchesValue("driver", network.Driver) ||
			!filters.matchesLabels(network.Labels) {
			continue
		}
		response = append(response, dockerNetwork(network))
	}
	writeDockerJSON(w, http.StatusOK, response)
}

func (a *API) createNetwork(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		Internal   bool              `json:"Internal"`
		EnableIPv6 bool              `json:"EnableIPv6"`
		Options    map[string]string `json:"Options"`
		Labels     map[string]string `json:"Labels"`
		IPAM       struct {
			Config []struct {
				Subnet  string `json:"Subnet"`
				Gateway string `json:"Gateway"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	subnets := make([]string, 0, len(request.IPAM.Config))
	gateways := make([]string, 0, len(request.IPAM.Config))
	for _, config := range request.IPAM.Config {
		if config.Subnet != "" {
			subnets = append(subnets, config.Subnet)
		}
		if config.Gateway != "" {
			gateways = append(gateways, config.Gateway)
		}
	}
	id, err := a.manager.CreateNetwork(r.Context(), CreateNetworkRequest{
		Name: request.Name, Driver: request.Driver, Subnets: subnets, Gateways: gateways,
		Internal: request.Internal, EnableIPv6: request.EnableIPv6,
		Options: request.Options, Labels: request.Labels,
	})
	if err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusCreated, map[string]any{"Id": id, "Warning": ""})
}

func (a *API) inspectNetwork(w http.ResponseWriter, r *http.Request) {
	value, err := a.manager.InspectNetwork(r.Context(), r.PathValue("id"))
	writeDockerRaw(w, value, err)
}

func (a *API) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	if err := a.manager.RemoveNetwork(r.Context(), r.PathValue("id")); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) connectNetwork(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Container      string `json:"Container"`
		EndpointConfig struct {
			Aliases    []string          `json:"Aliases"`
			DriverOpts map[string]string `json:"DriverOpts"`
			IPAMConfig *struct {
				IPv4Address  string   `json:"IPv4Address"`
				IPv6Address  string   `json:"IPv6Address"`
				LinkLocalIPs []string `json:"LinkLocalIPs"`
			} `json:"IPAMConfig"`
		} `json:"EndpointConfig"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if len(request.EndpointConfig.DriverOpts) > 0 ||
		(request.EndpointConfig.IPAMConfig != nil &&
			(request.EndpointConfig.IPAMConfig.IPv4Address != "" ||
				request.EndpointConfig.IPAMConfig.IPv6Address != "" ||
				len(request.EndpointConfig.IPAMConfig.LinkLocalIPs) > 0)) {
		writeDockerUnsupported(w, "custom container network endpoint settings")
		return
	}
	if err := a.manager.ConnectNetwork(r.Context(), r.PathValue("id"), request.Container, request.EndpointConfig.Aliases); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) disconnectNetwork(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Container string `json:"Container"`
		Force     bool   `json:"Force"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if err := a.manager.DisconnectNetwork(r.Context(), r.PathValue("id"), request.Container, request.Force); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) volumes(w http.ResponseWriter, r *http.Request) {
	filters, err := parseDockerFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	volumes, err := a.manager.Volumes(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}
	response := make([]map[string]any, 0, len(volumes))
	for _, volume := range volumes {
		if !filters.matchesName(volume.Name) ||
			!filters.matchesValue("driver", volume.Driver) ||
			!filters.matchesLabels(volume.Labels) {
			continue
		}
		response = append(response, dockerVolume(volume))
	}
	writeDockerJSON(w, http.StatusOK, map[string]any{"Volumes": response, "Warnings": []string{}})
}

func (a *API) createVolume(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		DriverOpts map[string]string `json:"DriverOpts"`
		Labels     map[string]string `json:"Labels"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if len(request.DriverOpts) > 0 {
		writeDockerUnsupported(w, "volume driver options")
		return
	}
	volume, err := a.manager.CreateVolume(r.Context(), request.Name, request.Driver, request.Labels)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusCreated, dockerVolume(volume))
}

func (a *API) inspectVolume(w http.ResponseWriter, r *http.Request) {
	value, err := a.manager.InspectVolume(r.Context(), r.PathValue("name"))
	writeDockerRaw(w, value, err)
}

func (a *API) deleteVolume(w http.ResponseWriter, r *http.Request) {
	if err := a.manager.RemoveVolume(r.Context(), r.PathValue("name"), dockerBool(r, "force")); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
