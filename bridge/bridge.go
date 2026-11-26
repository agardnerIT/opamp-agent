// Package bridge is the commodity layer: everything product-agnostic about
// being an OpAMP-managed agent. It owns the OpAMP transport, identity,
// health reporting, effective-config reporting, and — critically — the
// last-known-good (LKG) orchestration around render → apply → verify.
//
// The bridge knows nothing about any product. It is told what to do through
// the six-verb driver.Driver interface.
package bridge

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/agardnerIT/opamp-agent/driver"
	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// Options configure one bridge run.
type Options struct {
	ServerURL string // OpAMP endpoint, ws(s):// or http(s)://
	Token     string // agent bearer token
	// InstanceUIDFile persists this host's agent identity across restarts;
	// created if missing. Default "instance.uid" in the working directory.
	InstanceUIDFile string
	// Logger receives bridge-internal diagnostics; nil = quiet.
	Logger Logger
}

// Logger is the bridge's optional diagnostic sink (opamp-go logs come through
// here too — silencing it during bring-up cost us half an hour once; leave it on).
type Logger interface {
	Debugf(format string, v ...any)
	Errorf(format string, v ...any)
}

// Run blocks until ctx is done: connects to the server, enrolls as the
// driver's identity, and services remote config through the driver.
func Run(ctx context.Context, opts Options, drv driver.Driver) error {
	if opts.Token == "" {
		return fmt.Errorf("bridge: agent token required (-token or OPAMP_TOKEN)")
	}
	if opts.InstanceUIDFile == "" {
		opts.InstanceUIDFile = "instance.uid"
	}

	uid, err := loadOrCreateUID(opts.InstanceUIDFile)
	if err != nil {
		return fmt.Errorf("bridge: instance uid: %w", err)
	}

	agent := client.NewWebSocket(loggerAdapter{opts.Logger}) // *wsClient, satisfies client.OpAMPClient
	var _ = agent

	ident := drv.Identity()
	attrs := []*protobufs.KeyValue{
		{Key: "service.name", Value: anyStr(ident.ServiceName)},
	}
	if ident.Version != "" {
		attrs = append(attrs, &protobufs.KeyValue{Key: ident.ServiceName + ".version", Value: anyStr(ident.Version)})
	}
	for k, v := range ident.Identifying {
		attrs = append(attrs, &protobufs.KeyValue{Key: k, Value: anyStr(v)})
	}
	nonIdent := make([]*protobufs.KeyValue, 0, len(ident.NonIdentifying))
	for k, v := range ident.NonIdentifying {
		nonIdent = append(nonIdent, &protobufs.KeyValue{Key: k, Value: anyStr(v)})
	}
	// opamp-go requires description and health to be set before Start when
	// those capabilities are advertised.
	if err := agent.SetAgentDescription(&protobufs.AgentDescription{
		IdentifyingAttributes:    attrs,
		NonIdentifyingAttributes: nonIdent,
	}); err != nil {
		return fmt.Errorf("bridge: agent description: %w", err)
	}
	if err := agent.SetHealth(toProtoHealth(drv.Health(context.Background()))); err != nil {
		return fmt.Errorf("bridge: initial health: %w", err)
	}

	state := &bridgeState{drv: drv, agent: agent}

	settings := types.StartSettings{
		OpAMPServerURL: opts.ServerURL,
		InstanceUid:    uid,
		Header:         map[string][]string{"Authorization": {"Bearer " + opts.Token}},
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
			protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig,
		Callbacks: types.Callbacks{
			OnConnect: func(ctx context.Context) {
				// product state may have changed while disconnected
				_ = agent.SetHealth(toProtoHealth(drv.Health(ctx)))
			},
			OnMessage: func(ctx context.Context, msg *types.MessageData) {
				state.onRemoteConfig(ctx, msg)
			},
			GetEffectiveConfig: func(ctx context.Context) (*protobufs.EffectiveConfig, error) {
				eff := map[string]*protobufs.AgentConfigObject{}
				for k, v := range drv.EffectiveConfig(ctx) {
					eff[k] = &protobufs.AgentConfigObject{Body: v}
				}
				return &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{ConfigMap: eff}}, nil
			},
		},
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.Start(runCtx, settings); err != nil {
		return fmt.Errorf("bridge: start: %w", err)
	}
	<-runCtx.Done()
	return agent.Stop(ctx)
}

// ---- remote-config pipeline: snapshot → render → apply → verify → LKG ----

type bridgeState struct {
	drv             driver.Driver
	agent           client.OpAMPClient
	lastAppliedHash []byte
}

func (s *bridgeState) onRemoteConfig(ctx context.Context, msg *types.MessageData) {
	rc := msg.RemoteConfig
	if rc == nil || rc.Config == nil {
		return
	}
	hash := rc.ConfigHash
	var body []byte
	for _, cfg := range rc.Config.ConfigMap {
		body = cfg.Body
		break // single-artifact drivers for now; multi-object is a Phase-2 question
	}
	status, reason, skipped := s.applyRemoteConfig(ctx, hash, body)
	if skipped {
		return
	}

	_ = s.agent.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: hash,
		Status:               status,
		ErrorMessage:         reason,
	})
	_ = s.agent.UpdateEffectiveConfig(ctx)
	fmt.Printf("[opamp-agent] config %x -> %s %s\n", hash[:min(4, len(hash))], status, reason)
}

// applyRemoteConfig runs the render → apply → verify pipeline and records the
// applied hash. It is split out of onRemoteConfig so the pipeline is testable
// without an OpAMP server or a live agent.
//
// skipped means "nothing to do": no hash, or the same config hash we already
// applied. Comparing the hash (not the body) is what makes the server's
// repeated RemoteConfig a no-op.
func (s *bridgeState) applyRemoteConfig(ctx context.Context, hash, body []byte) (status protobufs.RemoteConfigStatuses, reason string, skipped bool) {
	if len(hash) == 0 || string(hash) == string(s.lastAppliedHash) {
		return 0, "", true
	}

	status, reason = s.renderApplyVerify(ctx, body)
	if status == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED {
		s.lastAppliedHash = append([]byte(nil), hash...)
	}
	return status, reason, false
}

func (s *bridgeState) renderApplyVerify(ctx context.Context, body []byte) (protobufs.RemoteConfigStatuses, string) {
	ident := s.drv.Identity()
	prev := snapshotFiles(ident.OwnedFiles)

	rendered, err := s.drv.Render(body)
	if err != nil {
		return protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "render: " + err.Error()
	}
	// The bridge owns artifact writes: driver.Render decides what goes where,
	// the bridge puts it there (and can put the LKG copy back). Drivers only
	// activate what the bridge has written. Write happens BEFORE Validate so
	// validators that inspect the on-disk file (promtool, nginx -t, apachectl
	// configtest) see the NEW content, not the previous one.
	for _, a := range rendered.Artifacts {
		if err := os.WriteFile(a.Path, a.Body, 0o644); err != nil {
			return protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "write artifact: " + err.Error()
		}
	}
	if err := s.drv.Validate(rendered); err != nil && err != driver.ErrUnsupported {
		restoreFiles(prev) // validation failure: never apply, never disarm
		return protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "validate: " + err.Error()
	}
	if err := s.drv.Apply(ctx, rendered); err != nil {
		// A transport-level apply error means the product probably never saw
		// the new content; put the last-known-good files back so disk still
		// reflects what is running (and so the next reconnect starts clean).
		restoreFiles(prev)
		return protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "apply: " + err.Error()
	}
	applied, reason := s.drv.Verify(ctx, rendered)
	if applied {
		return protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, ""
	}

	// A rejected push MUST NOT leave the product disarmed: restore
	// last-known-good and re-apply before reporting FAILED. Without this, a
	// product with no validate seam and a lying apply response can be left
	// with an empty/disabled working set. If the driver declared no
	// OwnedFiles, there is nothing to
	// restore and the reason says so.
	if len(prev) == 0 {
		return protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED,
			reason + " (no LKG: driver owns no files)"
	}
	restoreFiles(prev)
	if err := s.drv.Apply(ctx, rendered); err == nil {
		if ok, _ := s.drv.Verify(ctx, rendered); ok {
			reason += " (rolled back to last-known-good)"
			return protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, reason
		}
	}
	return protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED,
		reason + " (LKG ROLLBACK FAILED — manual intervention required)"
}

// ---- small helpers ----

func snapshotFiles(paths []string) map[string][]byte {
	prev := make(map[string][]byte, len(paths))
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			prev[p] = b
		}
	}
	return prev
}

func restoreFiles(prev map[string][]byte) {
	for p, b := range prev {
		_ = os.WriteFile(p, b, 0o644)
	}
}

func toProtoHealth(h driver.Health) *protobufs.ComponentHealth {
	ph := &protobufs.ComponentHealth{Healthy: h.Healthy}
	if !h.StartTime.IsZero() {
		ph.StartTimeUnixNano = uint64(h.StartTime.UnixNano())
	}
	_ = h.Detail
	return ph
}

// loadOrCreateUID reads a 16-byte instance uid, or mints one from crypto/rand
// and persists it. Identity must survive restarts (it is the server's key for
// this host) and must not be guessable or collide across hosts sharing a clock,
// so the bytes come from the OS CSPRNG.
func loadOrCreateUID(path string) (types.InstanceUid, error) {
	var uid types.InstanceUid
	if b, err := os.ReadFile(path); err == nil && len(b) == len(uid) {
		copy(uid[:], b)
		return uid, nil
	}
	if _, err := rand.Read(uid[:]); err != nil {
		return uid, fmt.Errorf("generate uid: %w", err)
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return uid, fmt.Errorf("create uid directory: %w", err)
		}
	}
	if err := os.WriteFile(path, uid[:], 0o600); err != nil {
		return uid, err
	}
	return uid, nil
}

func anyStr(s string) *protobufs.AnyValue {
	return &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: s}}
}

// loggerAdapter adapts our optional Logger to opamp-go's types.Logger
// (which mandates context parameters).
type loggerAdapter struct{ l Logger }

func (a loggerAdapter) Debugf(_ context.Context, format string, v ...any) {
	if a.l != nil {
		a.l.Debugf(format, v...)
	}
}
func (a loggerAdapter) Errorf(_ context.Context, format string, v ...any) {
	if a.l != nil {
		a.l.Errorf(format, v...)
	}
}
