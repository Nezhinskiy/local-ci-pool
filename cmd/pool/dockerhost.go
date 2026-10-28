package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// dockerHost returns the host the docker CLI would talk to, or "" to leave the
// choice to the client's default. Under launchd DOCKER_HOST is unset and
// /var/run/docker.sock may not be linked, so the pool follows the CLI's own
// rules: DOCKER_HOST, then the current docker context, then the first of
// sockets that exists.
func dockerHost(getenv func(string) string, home string, sockets []string) string {
	if h := getenv("DOCKER_HOST"); h != "" {
		return h
	}
	cfg := getenv("DOCKER_CONFIG")
	if cfg == "" {
		cfg = filepath.Join(home, ".docker")
	}
	if h := contextHost(cfg, currentContext(getenv, cfg)); h != "" {
		return h
	}
	for _, s := range sockets {
		if _, err := os.Stat(s); err == nil {
			return "unix://" + s
		}
	}
	return ""
}

// currentContext is the context name DOCKER_CONTEXT gives, else the one in
// <cfg>/config.json; "" when neither names one.
func currentContext(getenv func(string) string, cfg string) string {
	if c := getenv("DOCKER_CONTEXT"); c != "" {
		return c
	}
	b, err := os.ReadFile(filepath.Join(cfg, "config.json"))
	if err != nil {
		return ""
	}
	var c struct {
		CurrentContext string `json:"currentContext"`
	}
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	return c.CurrentContext
}

// contextHost is the docker endpoint of the named context, read from
// <cfg>/contexts/meta/<hex sha256 of the name>/meta.json. The default context
// has no file, and an unreadable one gives "".
func contextHost(cfg, name string) string {
	if name == "" || name == "default" {
		return ""
	}
	sum := sha256.Sum256([]byte(name))
	b, err := os.ReadFile(filepath.Join(cfg, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Endpoints struct {
			Docker struct {
				Host string
			} `json:"docker"`
		}
	}
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.Endpoints.Docker.Host
}
