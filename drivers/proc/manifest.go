// Package proc is the manifest-driven driver: one declarative YAML file is
// the entire vendor artifact for a "config file + reload/health endpoint"
// product. No product-specific code exists here — the manifest is the only
// product knowledge.
//
// Security stance: the manifest is a strict allowlist (unknown fields are a
// load error), commands are fixed argv from the manifest (never from the
// server), and the only substitution is the manifest's own config path.
// Manifests are selected locally, never server-pushed.
package proc

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Manifest struct {
	Driver   string `yaml:"driver"` // must be "proc" (allowlist discipline)
	Identity struct {
		ServiceName string `yaml:"service_name"`
	} `yaml:"identity"`
	Config struct {
		Path string `yaml:"path"`
	} `yaml:"config"`
	Apply struct {
		Method string `yaml:"method"` // http_endpoint | signal
		// http_endpoint method
		Endpoint string `yaml:"endpoint"`
		// signal method
		Signal  string `yaml:"signal"`  // e.g. SIGHUP
		PidFile string `yaml:"pid_file"`
	} `yaml:"apply"`
	Validate struct {
		// Fixed argv; "{config_path}" is replaced with Config.Path.
		// Empty means honest ErrUnsupported.
		Command []string `yaml:"command"`
	} `yaml:"validate"`
	Report struct {
		HealthEndpoint string `yaml:"health_endpoint"`
	} `yaml:"report"`
}

func LoadManifest(path string) (Manifest, error) {
	var m Manifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // unknown fields are a load error: strict allowlist
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("manifest %s: %w", path, err)
	}
	if m.Driver != "proc" {
		return m, fmt.Errorf("manifest %s: driver must be \"proc\", got %q", path, m.Driver)
	}
	if m.Identity.ServiceName == "" || m.Config.Path == "" {
		return m, fmt.Errorf("manifest %s: identity.service_name and config.path are required", path)
	}
	switch m.Apply.Method {
	case "http_endpoint":
		if m.Apply.Endpoint == "" {
			return m, fmt.Errorf("manifest %s: apply.endpoint required for http_endpoint", path)
		}
	case "signal":
		if m.Apply.Signal == "" || m.Apply.PidFile == "" {
			return m, fmt.Errorf("manifest %s: apply.signal and apply.pid_file required for signal", path)
		}
	default:
		return m, fmt.Errorf("manifest %s: apply.method must be http_endpoint or signal", path)
	}
	if m.Report.HealthEndpoint == "" {
		return m, fmt.Errorf("manifest %s: report.health_endpoint is required", path)
	}
	return m, nil
}
