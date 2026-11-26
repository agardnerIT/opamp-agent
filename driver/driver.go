// Package driver is Interface 2 — the entire per-product surface of the
// opamp-agent bridge. Everything else (OpAMP transport, heartbeats, LKG,
// rollback, status reporting) lives in the bridge and is shared by all
// products. A vendor answers six questions here; that is the whole
// integration.
package driver

import (
	"context"
	"errors"
	"time"
)

// ErrUnsupported tells the bridge a seam genuinely does not exist for this
// product (e.g. no pre-apply validation). The bridge degrades honestly.
var ErrUnsupported = errors.New("unsupported by this agent")

// Identity is declared before the agent connects; it feeds AgentDescription
// and the bridge-side LKG machinery.
type Identity struct {
	ServiceName string
	Version     string // may be empty if the product's version is unreachable
	// OwnedFiles are the paths the driver owns on disk. The bridge snapshots
	// them before every render and restores them when verification fails —
	// the last-known-good guarantee.
	OwnedFiles []string
	// Extra attributes for AgentDescription.
	Identifying    map[string]string
	NonIdentifying map[string]string
}

// Artifact is one file the driver rendered from a remote config.
type Artifact struct {
	Path string
	Body []byte
}

// Rendered is the full set of artifacts produced by one Render call.
type Rendered struct {
	Artifacts []Artifact
}

// Health is the product's health as observed externally (socket, endpoint,
// process). Zero StartTime means unknown.
type Health struct {
	Healthy   bool
	Detail    string
	StartTime time.Time
}

// Driver is the six-verb contract. Each method answers one question about
// the product; none of them know OpAMP exists.
type Driver interface {
	// Identity: "who are you, and which files do you own?"
	Identity() Identity

	// Render: "given the remote config, what goes on disk?"
	Render(cfg []byte) (Rendered, error)

	// Validate: "will the product accept this before we apply it?"
	// Return ErrUnsupported when the product has no such check — apply-time
	// verification is then the only guard (and LKG recovery is mandatory).
	Validate(r Rendered) error

	// Apply: "how does the config take effect?" (signal / reload API / socket)
	Apply(ctx context.Context, r Rendered) error

	// Verify: "did it ACTUALLY take effect?" — apply responses can lie
	// (some products report success even for invalid configs). Returns
	// a human-readable reason on failure.
	Verify(ctx context.Context, r Rendered) (applied bool, reason string)

	// Health: "is the product alive, and since when?"
	Health(ctx context.Context) Health

	// EffectiveConfig: "what should be reported as running?" May be hashes
	// rather than bodies (stated, not claimed).
	EffectiveConfig(ctx context.Context) map[string][]byte
}
