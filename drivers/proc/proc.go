package proc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agardnerIT/opamp-agent/driver"
)

// Driver implements driver.Driver for any manifest-described product.
type Driver struct {
	m    Manifest
	last *applyResult // the apply response, remembered for Verify
}

type applyResult struct {
	status int
	body   string
}

func New(m Manifest) *Driver { return &Driver{m: m} }

// identity: from the manifest, nothing else.
func (d *Driver) Identity() driver.Identity {
	return driver.Identity{
		ServiceName: d.m.Identity.ServiceName,
		OwnedFiles:  []string{d.m.Config.Path},
	}
}

// render: the remote config replaces the config file. The proc driver is a
// single-file driver: it takes the single entry explicitly (by key when the
// server names one, otherwise the sole entry) and refuses a multi-object push
// it has no way to place.
func (d *Driver) Render(cfg driver.Config) (driver.Rendered, error) {
	if len(cfg) != 1 {
		return driver.Rendered{}, fmt.Errorf("proc driver manages one config file; got %d config objects", len(cfg))
	}
	var body []byte
	for _, b := range cfg {
		body = b
	}
	if len(body) == 0 {
		return driver.Rendered{}, fmt.Errorf("empty config body")
	}
	return driver.Rendered{Artifacts: []driver.Artifact{
		{Path: d.m.Config.Path, Body: body},
	}}, nil
}

// validate: run the manifest's fixed argv ({config_path} substituted from
// the manifest itself). Empty command = honest ErrUnsupported.
func (d *Driver) Validate(r driver.Rendered) error {
	argv := d.m.Validate.Command
	if len(argv) == 0 {
		return driver.ErrUnsupported
	}
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = strings.ReplaceAll(a, "{config_path}", d.m.Config.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		summary := strings.TrimSpace(string(out))
		if len(summary) > 400 {
			summary = summary[len(summary)-400:]
		}
		return fmt.Errorf("validate failed: %s", summary)
	}
	return nil
}

// apply: trigger the product's apply mechanism. The HTTP response is
// remembered as the raw material for Verify — the apply step itself only
// fails on transport errors, never on the product's opinion.
func (d *Driver) Apply(ctx context.Context, r driver.Rendered) error {
	switch d.m.Apply.Method {
	case "http_endpoint":
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Post(d.m.Apply.Endpoint, "text/plain", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		d.last = &applyResult{status: resp.StatusCode, body: strings.TrimSpace(string(buf[:n]))}
		return nil
	case "signal":
		raw, err := os.ReadFile(d.m.Apply.PidFile)
		if err != nil {
			return fmt.Errorf("pid file: %w", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return fmt.Errorf("pid file parse: %w", err)
		}
		sig := map[string]syscall.Signal{
			"SIGHUP": syscall.SIGHUP, "SIGUSR1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2,
		}[d.m.Apply.Signal]
		if sig == 0 {
			return fmt.Errorf("unsupported signal %q", d.m.Apply.Signal)
		}
		d.last = nil
		return syscall.Kill(pid, sig)
	}
	return fmt.Errorf("unknown apply method %q", d.m.Apply.Method)
}

// verify: for http_endpoint applies the response status/body IS the verdict
// (a failed reload returns 4xx/5xx with the parse error); afterwards the
// health endpoint must still be green. Signal applies can only check health.
func (d *Driver) Verify(ctx context.Context, r driver.Rendered) (bool, string) {
	if d.last != nil && d.last.status >= 400 {
		reason := d.last.body
		if reason == "" {
			reason = fmt.Sprintf("reload endpoint returned %d", d.last.status)
		}
		d.last = nil
		return false, "reload rejected: " + reason
	}
	d.last = nil
	if code, _, err := d.get(ctx, d.m.Report.HealthEndpoint); err != nil {
		return false, "health check failed: " + err.Error()
	} else if code >= 400 {
		return false, fmt.Sprintf("health endpoint returned %d after apply", code)
	}
	return true, ""
}

// health: the manifest's health endpoint.
func (d *Driver) Health(ctx context.Context) driver.Health {
	code, body, err := d.get(ctx, d.m.Report.HealthEndpoint)
	if err != nil {
		return driver.Health{Healthy: false, Detail: err.Error()}
	}
	if code >= 400 {
		return driver.Health{Healthy: false, Detail: fmt.Sprintf("health endpoint %d: %s", code, body)}
	}
	return driver.Health{Healthy: true, Detail: body}
}

// effective config: hash of the owned file — stated, not claimed. The key
// includes the service name so the server can key revisions to match.
func (d *Driver) EffectiveConfig(ctx context.Context) map[string][]byte {
	b, err := os.ReadFile(d.m.Config.Path)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(b)
	key := fmt.Sprintf("%s.config.sha256", d.m.Identity.ServiceName)
	return map[string][]byte{key: sum[:]}
}

func (d *Driver) get(ctx context.Context, url string) (int, string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, strings.TrimSpace(string(buf[:n])), nil
}
