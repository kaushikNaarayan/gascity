package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// bdLedgerProbeTimeout bounds one readability probe (a var so tests can shrink it). A timeout is not a schema
// finding, so it never turns a healthy check red.
var bdLedgerProbeTimeout = 30 * time.Second

// bdCLILedgerProbe reads one issue through the bd CLI, in the target scope,
// with the binary, environment and working directory `gc bd` itself uses.
//
// It deliberately does NOT read through gc's native store. On a v59 ledger the
// native store opens and lists fine (it knows v59), while the pinned bd rc.2
// CLI, the reader every agent actually uses, fails with "column storage_class
// could not be found". A probe through the native store therefore reports
// green on exactly the ledger that is broken (gcf-tf22, qa G-UPGRADE).
//
// Returns nil when the scope is readable OR when the probe cannot be run at
// all (non-bd provider, unresolvable bd binary): those are owned by other
// checks and must not add noise here.
func bdCLILedgerProbe(cityPath string, cfg *config.City, target execStoreTarget) error {
	if !providerUsesBdStoreContract(rawBeadsProviderForScope(target.ScopeRoot, cityPath)) {
		return nil
	}
	bdPath, err := resolveBdBinaryForScope(cityPath, target.ScopeRoot)
	if err != nil {
		return nil //nolint:nilerr // the bd binary checks own an unresolvable bd
	}
	env, err := bdCommandEnv(cityPath, cfg, target)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), bdLedgerProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bdPath, "list", "--json", "--limit", "1")
	cmd.Dir = target.ScopeRoot
	cmd.Env = workQueryEnvForDir(env, cmd.Dir)
	// Kill the whole process group on timeout and stop waiting on inherited
	// pipes: a hung bd's children would otherwise hold stdout open and stall the
	// probe for their entire lifetime.
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("bd list readability probe timed out after %s", bdLedgerProbeTimeout)
	}
	if runErr != nil {
		// bd fails loudly here: exit 1 with the error on stderr, or as a JSON
		// {"error": ...} object on stdout under --json.
		msg := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return errors.New("bd list readability probe failed: " + truncateProbeText(msg, 600))
	}
	// Exit 0 can still carry an error object under --json.
	var obj struct {
		Error string `json:"error"`
	}
	if t := strings.TrimSpace(stdout.String()); strings.HasPrefix(t, "{") && json.Unmarshal([]byte(t), &obj) == nil && obj.Error != "" {
		return errors.New("bd list readability probe failed: " + truncateProbeText(obj.Error, 600))
	}
	return nil
}

func truncateProbeText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// newBeadsStoreDoctorCheck is the production beads-store check. Its readability
// probe reads through the bd CLI, not the native store the check opens: the
// native store opens and lists fine on a ledger the pinned bd cannot read
// (gcf-tf22). Kept as a constructor so tests can build exactly what doctor runs.
func newBeadsStoreDoctorCheck(cityPath string, cfg *config.City, open func(string) (beads.StoreOpenResult, error)) *doctor.BeadsStoreCheck {
	return doctor.NewBeadsStoreCheck(cityPath, open).WithLedgerProbe(func(string) error {
		return bdCLILedgerProbe(cityPath, cfg, bdCityScopeTarget(cityPath, cfg))
	})
}

// newRigBeadsDoctorCheck is the production rig:<name>:beads check; see
// newBeadsStoreDoctorCheck.
func newRigBeadsDoctorCheck(cityPath string, cfg *config.City, rig config.Rig, open func(string) (beads.Store, error)) *doctor.RigBeadsCheck {
	return doctor.NewRigBeadsCheck(cityPath, rig, open).WithLedgerProbe(func(string) error {
		return bdCLILedgerProbe(cityPath, cfg, bdRigScopeTarget(cityPath, rig))
	})
}
