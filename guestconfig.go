package main

import (
	"encoding/json"

	"github.com/popstarapps/john/sandbox"
)

// jsonUnmarshalConfig decodes the configuration the loopback machine passes as
// an argument. It lives outside both mains because both of them run the agent.
func jsonUnmarshalConfig(raw string, cfg *sandbox.GuestConfig) error {
	return json.Unmarshal([]byte(raw), cfg)
}
