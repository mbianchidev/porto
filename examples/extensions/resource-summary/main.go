//go:build wasip1 && wasm

package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
)

func main() {
	var input struct {
		Action     string `json:"action"`
		ResourceID string `json:"resourceId"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		fmt.Fprintln(os.Stderr, "invalid invocation")
		os.Exit(1)
	}
	path := "/api/docker/containers"
	if input.Action == "inspect" {
		if input.ResourceID == "" {
			fmt.Fprintln(os.Stderr, "explicit container required")
			os.Exit(1)
		}
		path += "/" + url.PathEscape(input.ResourceID) + "/identity"
	} else if input.Action != "inventory" {
		fmt.Fprintln(os.Stderr, "unknown action")
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"capability": "runtime.read", "method": "GET", "path": path}); err != nil {
		os.Exit(1)
	}
}
