package main

import (
	"embed"
	"errors"
	"io/fs"
)

// embedded holds hyperhand-agent.exe when the build placed it in cmd/hyperhand/embedded (the release workflow does;
// development builds put the agent beside hyperhand.exe instead); a plain go build embeds only the directory's README.
// A leftover agent there is embedded into every later build and fails TestPlainBuildEmbedsNoAgent: delete it.
//
//go:embed embedded
var embedded embed.FS

// embeddedAgent returns the embedded guest agent, or nil when this build does not contain one.
func embeddedAgent() ([]byte, error) {
	data, err := embedded.ReadFile("embedded/hyperhand-agent.exe")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}
