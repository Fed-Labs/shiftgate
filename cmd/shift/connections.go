package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"shift.dev/shift/internal/endpoint"
	"shift.dev/shift/internal/model"
)

type connectionProfile struct {
	Alias      string `json:"alias"`
	MachineID  string `json:"machine_id"`
	PeerURL    string `json:"peer_url"`
	ServerName string `json:"server_name"`
}

type connectionStore struct {
	Version     int                 `json:"version"`
	Connections []connectionProfile `json:"connections"`
}

func connectionStorePath() string {
	if value := os.Getenv("SHIFT_CONNECTIONS_FILE"); value != "" {
		return value
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "shift-connections.json")
	}
	return filepath.Join(directory, "shift", "connections.json")
}

func loadConnections() (connectionStore, error) {
	path := connectionStorePath()
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return connectionStore{Version: 1}, nil
	}
	if err != nil {
		return connectionStore{}, err
	}
	var store connectionStore
	if err := json.Unmarshal(encoded, &store); err != nil {
		return connectionStore{}, fmt.Errorf("read connections: %w", err)
	}
	if store.Version != 1 {
		return connectionStore{}, errors.New("unsupported connections file version")
	}
	return store, nil
}

func saveConnections(store connectionStore) error {
	path := connectionStorePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

func runConnect(ctx context.Context, settings options, arguments []string) error {
	_ = ctx
	flags := flagNew("connect")
	peerURL := flags.String("to", "", "public HTTPS peer endpoint (not a local agent socket)")
	machineID := flags.String("machine-id", "", "real destination machine identity")
	serverName := flags.String("server-name", "", "certificate DNS name (defaults to URL hostname)")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) != 1 {
		return usageError("usage: shiftgate connect ALIAS --to https://HOST:PORT --machine-id ID [--server-name NAME]")
	}
	alias := strings.TrimSpace(positional[0])
	if alias == "" || strings.ContainsAny(alias, " /\\\t\r\n") {
		return usageError("alias must be one word, for example gpu-west")
	}
	if *machineID == "" {
		return usageError("--machine-id is required; run shiftgate machine on the destination and copy its identity")
	}
	normalized, err := endpoint.Normalize(*peerURL)
	if err != nil {
		return usageError(err.Error())
	}
	name, err := endpoint.ServerName(normalized, *serverName)
	if err != nil {
		return usageError(err.Error())
	}
	store, err := loadConnections()
	if err != nil {
		return err
	}
	profile := connectionProfile{Alias: alias, MachineID: *machineID, PeerURL: normalized, ServerName: name}
	replaced, found := "", false
	for index := range store.Connections {
		if strings.EqualFold(store.Connections[index].Alias, alias) {
			replaced, found = store.Connections[index].PeerURL, true
			store.Connections[index] = profile
		}
	}
	if !found {
		store.Connections = append(store.Connections, profile)
	}
	if err := saveConnections(store); err != nil {
		return err
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, profile)
	}
	if found {
		_, _ = fmt.Fprintf(settings.stdout, "Replaced existing connection %s (was %s).\n", alias, replaced)
	} else {
		_, _ = fmt.Fprintf(settings.stdout, "Saved %s → %s (machine %s).\n", alias, normalized, *machineID)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Next: shiftgate migrate WORKLOAD --to %s --preflight\n", alias)
	_, _ = fmt.Fprintln(settings.stdout, "The destination still needs its public peer listener, valid mTLS certificates, and a firewall/NAT rule. The local Unix socket is never exposed.")
	return nil
}

func runConnections(settings options, arguments []string) error {
	if err := rejectArgs("connections", "shiftgate connections", arguments); err != nil {
		return err
	}
	store, err := loadConnections()
	if err != nil {
		return err
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, store.Connections)
	}
	if len(store.Connections) == 0 {
		_, _ = fmt.Fprintln(settings.stdout, "No remote machines saved. Add one with: shiftgate connect ALIAS --to https://HOST:8443 --machine-id ID")
		return nil
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ALIAS\tPEER URL\tMACHINE ID\tTLS NAME")
	for _, value := range store.Connections {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", value.Alias, value.PeerURL, value.MachineID, value.ServerName)
	}
	return writer.Flush()
}

func runDisconnect(settings options, arguments []string) error {
	if len(arguments) != 1 {
		return usageError("usage: shiftgate disconnect ALIAS")
	}
	store, err := loadConnections()
	if err != nil {
		return err
	}
	filtered := store.Connections[:0]
	removed := false
	for _, value := range store.Connections {
		if strings.EqualFold(value.Alias, arguments[0]) {
			removed = true
			continue
		}
		filtered = append(filtered, value)
	}
	if !removed {
		return exitError{code: 4, err: fmt.Errorf("no saved connection named %q; run 'shiftgate connections' to list them", arguments[0])}
	}
	store.Connections = filtered
	if err := saveConnections(store); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(settings.stdout, "Removed connection %s.\n", arguments[0])
	return nil
}

func resolveDestination(value, machineID, serverName string) (model.Destination, error) {
	if strings.HasPrefix(value, "https://") {
		normalized, err := endpoint.Normalize(value)
		if err != nil {
			return model.Destination{}, err
		}
		name, err := endpoint.ServerName(normalized, serverName)
		if err != nil {
			return model.Destination{}, err
		}
		return model.Destination{MachineID: machineID, AgentURL: normalized, ServerName: name}, nil
	}
	store, err := loadConnections()
	if err != nil {
		return model.Destination{}, err
	}
	for _, profile := range store.Connections {
		if strings.EqualFold(profile.Alias, value) {
			if machineID == "" {
				machineID = profile.MachineID
			}
			if serverName == "" {
				serverName = profile.ServerName
			}
			return model.Destination{MachineID: machineID, AgentURL: profile.PeerURL, ServerName: serverName}, nil
		}
	}
	return model.Destination{}, fmt.Errorf("remote alias %q is not saved; run shiftgate connect %s --to https://HOST:8443 --machine-id ID", value, value)
}
