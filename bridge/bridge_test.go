package bridge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agardnerIT/opamp-agent/driver"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// fakeDriver is a driver with no product: every verb is a switch the test
// throws. It is deliberately dumb — the pipeline, not the product, is under
// test here.
type fakeDriver struct {
	ident     driver.Identity
	artifacts []driver.Artifact

	renderErr   error
	validateErr error
	applyErr    error

	verifyOK     bool
	verifyReason string
	// verifySeq, when set, supplies per-call verdicts (a product that rejects
	// the new config and accepts the restored one).
	verifySeq []bool

	applyCalls  int
	verifyCalls int
	lastApplied []byte

	// onApply runs before the apply result is returned (lets a test make the
	// on-disk file look like the product rewrote it).
	onApply func()
}

func (f *fakeDriver) Identity() driver.Identity { return f.ident }

func (f *fakeDriver) Render(cfg []byte) (driver.Rendered, error) {
	if f.renderErr != nil {
		return driver.Rendered{}, f.renderErr
	}
	return driver.Rendered{Artifacts: f.artifacts}, nil
}

func (f *fakeDriver) Validate(driver.Rendered) error { return f.validateErr }

func (f *fakeDriver) Apply(_ context.Context, r driver.Rendered) error {
	f.applyCalls++
	if f.onApply != nil {
		f.onApply()
	}
	if f.applyErr != nil {
		return f.applyErr
	}
	if len(r.Artifacts) > 0 {
		f.lastApplied = r.Artifacts[0].Body
	}
	return nil
}

func (f *fakeDriver) Verify(context.Context, driver.Rendered) (bool, string) {
	f.verifyCalls++
	if n := len(f.verifySeq); n > 0 {
		ok := f.verifySeq[0]
		f.verifySeq = f.verifySeq[1:]
		if ok {
			return true, ""
		}
		return false, f.verifyReason
	}
	return f.verifyOK, f.verifyReason
}

func (f *fakeDriver) Health(context.Context) driver.Health { return driver.Health{Healthy: true} }

func (f *fakeDriver) EffectiveConfig(context.Context) map[string][]byte {
	return map[string][]byte{"fake.config.sha256": []byte("deadbeef")}
}

func newState(t *testing.T, f *fakeDriver) *bridgeState {
	t.Helper()
	return &bridgeState{drv: f}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const applied = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
const failed = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED

// A good config: render → write → (validate unsupported) → apply → verify.
func TestPipelineAppliesAndWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "product.yaml")
	writeFile(t, cfg, "old: true\n")

	f := &fakeDriver{
		ident:       driver.Identity{ServiceName: "fake", OwnedFiles: []string{cfg}},
		artifacts:   []driver.Artifact{{Path: cfg, Body: []byte("new: true\n")}},
		validateErr: driver.ErrUnsupported, // no pre-apply check: tolerated
		verifyOK:    true,
	}
	s := newState(t, f)

	status, reason, skipped := s.applyRemoteConfig(context.Background(), []byte("hash-1"), []byte("new: true\n"))
	if skipped {
		t.Fatal("first config must not be skipped")
	}
	if status != applied || reason != "" {
		t.Fatalf("want APPLIED/\"\", got %v/%q", status, reason)
	}
	if got := readFile(t, cfg); got != "new: true\n" {
		t.Fatalf("artifact not written, file = %q", got)
	}
	if f.applyCalls != 1 || f.verifyCalls != 1 {
		t.Fatalf("want 1 apply + 1 verify, got %d/%d", f.applyCalls, f.verifyCalls)
	}
	if string(s.lastAppliedHash) != "hash-1" {
		t.Fatalf("applied hash not recorded: %q", s.lastAppliedHash)
	}
}

// Verify says no: the file is restored, the LKG is re-applied and re-verified,
// and the status is FAILED with the product's reason.
func TestVerifyFailureRestoresLKGAndReapplies(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "product.yaml")
	writeFile(t, cfg, "good\n")

	f := &fakeDriver{
		ident:        driver.Identity{ServiceName: "fake", OwnedFiles: []string{cfg}},
		artifacts:    []driver.Artifact{{Path: cfg, Body: []byte("bad\n")}},
		validateErr:  driver.ErrUnsupported,
		verifyOK:     false,
		verifyReason: "reload rejected: parse error at line 2",
		verifySeq:    []bool{false, true}, // new config rejected, LKG accepted
	}
	f.onApply = func() {
		// the product "reads" the file and rewrites it; whatever it does, the
		// bridge must put the LKG content back.
		if readFile(t, cfg) == "bad\n" {
			writeFile(t, cfg, "product-mangled\n")
		}
	}
	s := newState(t, f)

	status, reason, _ := s.applyRemoteConfig(context.Background(), []byte("hash-bad"), []byte("bad\n"))
	if status != failed {
		t.Fatalf("want FAILED, got %v", status)
	}
	if !strings.Contains(reason, "reload rejected") {
		t.Fatalf("reason must carry the product's message, got %q", reason)
	}
	if !strings.Contains(reason, "rolled back to last-known-good") {
		t.Fatalf("reason must state the rollback, got %q", reason)
	}
	if got := readFile(t, cfg); got != "good\n" {
		t.Fatalf("LKG not restored, file = %q", got)
	}
	if f.applyCalls != 2 {
		t.Fatalf("want apply called twice (new + LKG), got %d", f.applyCalls)
	}
	if f.verifyCalls != 2 {
		t.Fatalf("want verify called twice (new + LKG), got %d", f.verifyCalls)
	}
	if len(s.lastAppliedHash) != 0 {
		t.Fatalf("a FAILED config must not become the applied hash, got %q", s.lastAppliedHash)
	}
}

// Validate says no: the apply step is never reached, and disk keeps the LKG.
func TestValidateFailureNeverApplies(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "product.yaml")
	writeFile(t, cfg, "good\n")

	f := &fakeDriver{
		ident:       driver.Identity{ServiceName: "fake", OwnedFiles: []string{cfg}},
		artifacts:   []driver.Artifact{{Path: cfg, Body: []byte("bad\n")}},
		validateErr: errors.New("Undefined macro"),
		verifyOK:    true,
	}
	s := newState(t, f)

	status, reason, _ := s.applyRemoteConfig(context.Background(), []byte("hash-bad"), []byte("bad\n"))
	if status != failed || !strings.Contains(reason, "validate:") || !strings.Contains(reason, "Undefined macro") {
		t.Fatalf("want FAILED with validator text, got %v/%q", status, reason)
	}
	if f.applyCalls != 0 {
		t.Fatalf("apply must not run after a validate failure, got %d calls", f.applyCalls)
	}
	if got := readFile(t, cfg); got != "good\n" {
		t.Fatalf("file not restored, = %q", got)
	}
}

// Apply transport error: FAILED and disk still shows the last-known-good.
func TestApplyErrorRestoresLKG(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "product.yaml")
	writeFile(t, cfg, "good\n")

	f := &fakeDriver{
		ident:       driver.Identity{ServiceName: "fake", OwnedFiles: []string{cfg}},
		artifacts:   []driver.Artifact{{Path: cfg, Body: []byte("bad\n")}},
		validateErr: driver.ErrUnsupported,
		applyErr:    errors.New("dial tcp 127.0.0.1:1234: connection refused"),
		verifyOK:    true,
	}
	s := newState(t, f)

	status, reason, _ := s.applyRemoteConfig(context.Background(), []byte("hash-bad"), []byte("bad\n"))
	if status != failed || !strings.Contains(reason, "apply:") {
		t.Fatalf("want FAILED with apply error, got %v/%q", status, reason)
	}
	if got := readFile(t, cfg); got != "good\n" {
		t.Fatalf("file not restored after apply error, = %q", got)
	}
	if f.verifyCalls != 0 {
		t.Fatalf("verify must not run after a failed apply, got %d", f.verifyCalls)
	}
}

// A driver that owns no files cannot be rolled back; the reason has to say so
// instead of pretending the product recovered.
func TestVerifyFailureWithNoOwnedFiles(t *testing.T) {
	f := &fakeDriver{
		ident:        driver.Identity{ServiceName: "fake"},
		artifacts:    nil,
		validateErr:  driver.ErrUnsupported,
		verifyOK:     false,
		verifyReason: "not ready",
	}
	s := newState(t, f)

	status, reason, _ := s.applyRemoteConfig(context.Background(), []byte("h"), []byte("x"))
	if status != failed {
		t.Fatalf("want FAILED, got %v", status)
	}
	if !strings.Contains(reason, "(no LKG: driver owns no files)") {
		t.Fatalf("reason must disclose the missing LKG, got %q", reason)
	}
}

// The server re-sends the same RemoteConfig on reconnect: no second apply.
func TestSameHashIsSkipped(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "product.yaml")
	writeFile(t, cfg, "old\n")

	f := &fakeDriver{
		ident:       driver.Identity{ServiceName: "fake", OwnedFiles: []string{cfg}},
		artifacts:   []driver.Artifact{{Path: cfg, Body: []byte("new\n")}},
		validateErr: driver.ErrUnsupported,
		verifyOK:    true,
	}
	s := newState(t, f)

	if st, _, skip := s.applyRemoteConfig(context.Background(), []byte("same-hash"), []byte("new\n")); skip || st != applied {
		t.Fatalf("first push: skip=%v status=%v", skip, st)
	}
	if _, _, skip := s.applyRemoteConfig(context.Background(), []byte("same-hash"), []byte("new\n")); !skip {
		t.Fatal("same config hash must be skipped")
	}
	if f.applyCalls != 1 {
		t.Fatalf("same hash must not re-apply, applyCalls = %d", f.applyCalls)
	}
}

// A failed push must not poison the dedupe state: the server retrying the same
// hash has to reach the driver again.
func TestFailedHashIsRetried(t *testing.T) {
	f := &fakeDriver{
		ident:       driver.Identity{ServiceName: "fake", OwnedFiles: []string{filepath.Join(t.TempDir(), "nope.yaml")}},
		validateErr: errors.New("bad"),
		verifyOK:    true,
	}
	s := newState(t, f)

	if _, _, skip := s.applyRemoteConfig(context.Background(), []byte("h"), []byte("x")); skip {
		t.Fatal("the first attempt must run")
	}
	if _, _, skip := s.applyRemoteConfig(context.Background(), []byte("h"), []byte("x")); skip {
		t.Fatal("a failed hash must be retried, not skipped")
	}
}

// Multi-artifact render: every file the driver names is written.
func TestMultiArtifactRenderWritesAllFiles(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one.yaml")
	two := filepath.Join(dir, "rules", "two.yaml")
	if err := os.MkdirAll(filepath.Dir(two), 0o755); err != nil {
		t.Fatal(err)
	}

	f := &fakeDriver{
		ident: driver.Identity{ServiceName: "fake", OwnedFiles: []string{one, two}},
		artifacts: []driver.Artifact{
			{Path: one, Body: []byte("a\n")},
			{Path: two, Body: []byte("b\n")},
		},
		validateErr: driver.ErrUnsupported,
		verifyOK:    true,
	}
	s := newState(t, f)

	if status, reason, _ := s.applyRemoteConfig(context.Background(), []byte("h"), []byte("x")); status != applied {
		t.Fatalf("want APPLIED, got %v/%q", status, reason)
	}
	if got := readFile(t, one); got != "a\n" {
		t.Fatalf("artifact one = %q", got)
	}
	if got := readFile(t, two); got != "b\n" {
		t.Fatalf("artifact two = %q", got)
	}
}

// Render errors are reported, not panicked on.
func TestRenderErrorIsReported(t *testing.T) {
	f := &fakeDriver{
		ident:     driver.Identity{ServiceName: "fake", OwnedFiles: nil},
		renderErr: errors.New("body is not YAML"),
		verifyOK:  true,
	}
	s := newState(t, f)

	status, reason, _ := s.applyRemoteConfig(context.Background(), []byte("h"), []byte("x"))
	if status != failed || !strings.Contains(reason, "render:") {
		t.Fatalf("want FAILED with render error, got %v/%q", status, reason)
	}
	if f.applyCalls != 0 {
		t.Fatalf("apply must not run after a render error, got %d", f.applyCalls)
	}
}

// ---- instance uid ----

func TestLoadOrCreateUIDIsRandomAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "instance.uid")
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("expected parent dir to be missing before first run")
	}

	a, err := loadOrCreateUID(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("parent dir was not created: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("uid file mode = %v, want 0600", info.Mode().Perm())
	}

	b, err := loadOrCreateUID(path)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("uid must be stable across calls")
	}

	other, err := loadOrCreateUID(filepath.Join(t.TempDir(), "instance.uid"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a[:], other[:]) {
		t.Fatal("two fresh ids must differ")
	}
	// A time-derived id repeats its byte pattern every 8 bytes (the old bug).
	if bytes.Equal(a[:8], a[8:]) {
		t.Fatal("uid bytes look time-derived, not random")
	}
}

func TestLoadOrCreateUIDReadsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.uid")
	want := []byte("0123456789abcdef")
	writeFile(t, path, string(want))

	got, err := loadOrCreateUID(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:], want) {
		t.Fatalf("existing uid overwritten: %x != %x", got[:], want)
	}
}

// A truncated/garbage file is replaced rather than read as a short id.
func TestLoadOrCreateUIDReplacesWrongSizeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.uid")
	writeFile(t, path, "short")

	got, err := loadOrCreateUID(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got[:], []byte("short")) {
		t.Fatal("short file must not be used as an id")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 16 {
		t.Fatalf("want rewritten 16-byte file, got %v/%v", info, err)
	}
}
