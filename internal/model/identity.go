package model

import "regexp"

var agentName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func ValidAgentName(name string) bool { return agentName.MatchString(name) }

type Identity struct {
	Agent    string `json:"agent,omitempty"`
	Admin    bool   `json:"admin"`
	Lead     bool   `json:"lead"`
	ChatAuth string `json:"chat_auth"`
}
