package controlplane

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/endpoint"
	"shift.dev/shift/internal/model"
)

func (server *Server) handleMachineList(writer http.ResponseWriter, request *http.Request) {
	records, err := server.database.Machines(request.Context(), request.PathValue("organizationID"), pageFromCappedRequest(request))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MACHINES_LOOKUP_FAILED", err)
		return
	}
	result := make([]Machine, 0, len(records))
	for _, record := range records {
		result = append(result, machineResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleMachineCreate(writer http.ResponseWriter, request *http.Request) {
	var input CreateMachineRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	normalizedAgentURL, err := validateMachineInput(input)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "MACHINE_INVALID", err.Error())
		return
	}
	id, err := model.NewID()
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ID_GENERATION_FAILED", err)
		return
	}
	capabilities, _ := json.Marshal(input.Capabilities)
	record, err := server.database.CreateMachine(request.Context(), database.MachineRecord{ID: id, OrganizationID: request.PathValue("organizationID"), MachineID: strings.TrimSpace(input.MachineID), Name: strings.TrimSpace(input.Name), AgentURL: normalizedAgentURL, LocalAgentURL: strings.TrimRight(strings.TrimSpace(input.LocalAgentURL), "/"), Capabilities: capabilities}, server.auditInput(request, "machine.create", "machine", id, map[string]any{"machine_id": strings.TrimSpace(input.MachineID)}))
	if err != nil {
		if errors.Is(err, database.ErrEntitlementExceeded) {
			writeError(writer, http.StatusPaymentRequired, "MACHINE_ENTITLEMENT_EXCEEDED", "organization machine entitlement is exhausted")
			return
		}
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "MACHINE_EXISTS", "machine is already registered")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MACHINE_CREATE_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusCreated, machineResponse(record))
}

func (server *Server) handleMachineHeartbeat(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Name         string         `json:"name"`
		AgentURL     string         `json:"agent_url"`
		Capabilities map[string]any `json:"capabilities"`
		Status       string         `json:"status"`
		// TransferBytes carries the bandwidth deltas the agent's meter
		// accumulated since its previous heartbeat (channel → bytes). Each
		// entry lands in usage_records against this organization, which the
		// request path already authenticated.
		TransferBytes map[string]int64 `json:"transfer_bytes"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	if input.Status == "" {
		input.Status = "online"
	}
	if input.Status != "online" && input.Status != "offline" && input.Status != "draining" {
		writeError(writer, http.StatusBadRequest, "MACHINE_STATUS_INVALID", "unsupported machine status")
		return
	}
	name := strings.TrimSpace(input.Name)
	if len(name) > 200 {
		writeError(writer, http.StatusBadRequest, "MACHINE_INVALID", "machine name is longer than 200 characters")
		return
	}
	agentURL := ""
	if strings.TrimSpace(input.AgentURL) != "" {
		normalized, err := normalizeAgentURL(input.AgentURL)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "MACHINE_INVALID", err.Error())
			return
		}
		agentURL = normalized
	}
	organizationID := request.PathValue("organizationID")
	for channel, bytes := range input.TransferBytes {
		if bytes <= 0 {
			continue
		}
		if err := server.database.RecordTransferBytes(request.Context(), organizationID, channel, bytes); err != nil {
			server.logger.Warn("could not record transfer usage", "organization_id", organizationID, "channel", channel, "error", err)
		}
	}
	var capabilities []byte
	if input.Capabilities != nil {
		capabilities, _ = json.Marshal(input.Capabilities)
	}
	record, err := server.database.UpdateMachineHeartbeat(request.Context(), request.PathValue("organizationID"), request.PathValue("machineID"), name, agentURL, capabilities, input.Status)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MACHINE_NOT_FOUND", "machine was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MACHINE_HEARTBEAT_FAILED", err)
		return
	}
	// The live-migration entitlement rides the heartbeat: managed agents learn
	// here whether their organization may perform live moves, so the agent's
	// admission gate can fail closed the same way the job route does. A lookup
	// error means "not allowed" — never open the gate on a database outage.
	liveAllowed, err := server.liveMigrationAllowed(request.Context(), organizationID)
	if err != nil {
		server.logger.Warn("live migration entitlement lookup failed", "organization_id", organizationID, "error", err)
		liveAllowed = false
	}
	writeJSON(writer, http.StatusOK, struct {
		Machine
		LiveMigrationAllowed bool `json:"live_migration_allowed"`
	}{machineResponse(record), liveAllowed})
}

// validateMachineInput rejects machine registrations without a stable id, a
// name, or an https peer URL. The optional local agent URL is a separate,
// explicitly configured dispatch path and is never inferred from the peer URL.
func validateMachineInput(input CreateMachineRequest) (string, error) {
	machineID := strings.TrimSpace(input.MachineID)
	name := strings.TrimSpace(input.Name)
	if machineID == "" || len(machineID) > 256 || name == "" || len(name) > 200 {
		return "", errors.New("machine id and name are required")
	}
	agentURL, err := normalizeAgentURL(input.AgentURL)
	if err != nil {
		return "", err
	}
	if local := strings.TrimSpace(input.LocalAgentURL); local != "" {
		parsed, err := url.Parse(local)
		if err != nil || parsed.Scheme != "unix" || parsed.Path == "" {
			return "", errors.New("local agent URL must be a unix socket URL")
		}
	}
	return agentURL, nil
}

const agentHostAllowlistEnv = "SHIFT_ALLOWED_AGENT_HOSTS"

var metadataAgentHosts = map[string]bool{
	"metadata":                   true,
	"metadata.google.internal":   true,
	"metadata.goog":              true,
	"metadata.tencentyun.com":    true,
	"instance-data":              true,
	"instance-data.ec2.internal": true,
}

var blockedAgentNetworks = parseAgentNetworks(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
)

func parseAgentNetworks(entries ...string) []*net.IPNet {
	networks := make([]*net.IPNet, 0, len(entries))
	for _, entry := range entries {
		if _, network, err := net.ParseCIDR(entry); err == nil {
			networks = append(networks, network)
		}
	}
	return networks
}

func normalizeAgentURL(raw string) (string, error) {
	normalized, err := endpoint.Normalize(raw)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", errors.New("agent URL is invalid")
	}
	host := parsed.Hostname()
	if host == "" {
		return "", errors.New("agent URL host is required")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", errors.New("agent URL port must be between 1 and 65535")
		}
	}
	if err := validateAgentHost(host); err != nil {
		return "", err
	}
	return normalized, nil
}

func validateAgentHost(host string) error {
	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if agentURLHostAllowed(normalized) {
		return nil
	}
	if metadataAgentHosts[normalized] {
		return errors.New("agent URL host is a cloud metadata endpoint")
	}
	if ip := net.ParseIP(normalized); ip != nil && isBlockedAgentIP(ip) {
		return errors.New("agent URL host is a link-local, loopback, private, or otherwise reserved address")
	}
	return nil
}

func isBlockedAgentIP(ip net.IP) bool {
	target := ip
	if v4 := ip.To4(); v4 != nil {
		target = v4
	}
	for _, network := range blockedAgentNetworks {
		if network.Contains(target) {
			return true
		}
	}
	return false
}

func agentURLHostAllowed(host string) bool {
	for _, entry := range strings.Split(os.Getenv(agentHostAllowlistEnv), ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == host {
			return true
		}
		if _, network, err := net.ParseCIDR(entry); err == nil {
			if ip := net.ParseIP(host); ip != nil && network.Contains(ip) {
				return true
			}
			continue
		}
		if entryIP := net.ParseIP(entry); entryIP != nil {
			if hostIP := net.ParseIP(host); hostIP != nil && entryIP.Equal(hostIP) {
				return true
			}
		}
	}
	return false
}

// handleMachineCapability answers a fleet query against the capability
// projection: which machines report this capability, filtered by kind and —
// for numbers — a minimum value. The kind is required because a capability
// name means different things in different documents, and guessing would
// answer a question the caller did not ask.
func (server *Server) handleMachineCapability(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	capability := strings.TrimSpace(query.Get("capability"))
	kind := strings.TrimSpace(query.Get("kind"))
	if capability == "" || len(capability) > 256 {
		writeError(writer, http.StatusBadRequest, "CAPABILITY_INVALID", "the capability query parameter is required")
		return
	}
	if kind != "number" && kind != "text" && kind != "boolean" {
		writeError(writer, http.StatusBadRequest, "CAPABILITY_KIND_INVALID", "kind must be number, text, or boolean")
		return
	}
	var minimum *float64
	if raw := strings.TrimSpace(query.Get("minimum")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || kind != "number" {
			writeError(writer, http.StatusBadRequest, "CAPABILITY_MINIMUM_INVALID", "minimum must be a number and is only valid with kind=number")
			return
		}
		minimum = &parsed
	}
	records, err := server.database.MachinesWithCapability(request.Context(), request.PathValue("organizationID"), capability, kind, minimum, pageFromCappedRequest(request))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "CAPABILITY_LOOKUP_FAILED", err)
		return
	}
	result := make([]Machine, 0, len(records))
	for _, record := range records {
		result = append(result, machineResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}
