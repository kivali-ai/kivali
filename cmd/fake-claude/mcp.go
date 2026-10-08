package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// mcpCallTimeout bounds one tool call end to end. The subagent tool
// answers with a receipt as soon as the job is queued, so this is a
// ceiling for a wedged server, not a wait anything relies on.
const mcpCallTimeout = 60 * time.Second

// coreServerName is the --mcp-config entry the real server registers.
const coreServerName = "kivali"

// callMCPTool starts the MCP server that --mcp-config names (the
// core server entry, else the first by name), performs the stdio JSON-RPC
// handshake the real CLI performs, calls one tool and returns its text
// content and whether it reported an error. Failures to reach the
// server come back as an error result, not a crash: that is what the
// model sees from the real CLI when an MCP server is down.
func callMCPTool(configPath, tool string, args map[string]any) (string, bool) {
	if configPath == "" {
		return "MCP server " + coreServerName + " is not connected (no --mcp-config)", true
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "MCP server " + coreServerName + " is not connected: " + err.Error(), true
	}
	var cfg struct {
		McpServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "MCP config unreadable: " + err.Error(), true
	}
	name := coreServerName
	srv, ok := cfg.McpServers[name]
	if !ok {
		names := make([]string, 0, len(cfg.McpServers))
		for n := range cfg.McpServers {
			names = append(names, n)
		}
		if len(names) == 0 {
			return "MCP config names no servers", true
		}
		sort.Strings(names)
		name = names[0]
		srv = cfg.McpServers[name]
	}

	ctx, cancel := context.WithTimeout(context.Background(), mcpCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
	cmd.Env = os.Environ()
	for k, v := range srv.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err.Error(), true
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err.Error(), true
	}
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("MCP server %s failed to start: %v", name, err), true
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	enc := json.NewEncoder(stdin)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	await := func(id int) (json.RawMessage, error) {
		for sc.Scan() {
			var resp struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &resp) != nil || resp.ID == nil || *resp.ID != id {
				continue // notifications and anything not ours
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("%s", resp.Error.Message)
			}
			return resp.Result, nil
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("MCP server %s closed its output", name)
	}

	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "fake-claude", "version": "0"},
		},
	}); err != nil {
		return err.Error(), true
	}
	if _, err := await(1); err != nil {
		return "MCP initialize failed: " + err.Error(), true
	}
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	}); err != nil {
		return err.Error(), true
	}
	res, err := await(2)
	if err != nil {
		return "MCP tools/call failed: " + err.Error(), true
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "MCP result unreadable: " + err.Error(), true
	}
	var parts []string
	for _, c := range out.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n"), out.IsError
}
