package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func newBeadsPreflightChecker(cityPath, provider string) contract.PreflightChecker {
	return contract.PreflightChecker{
		FS:                        fsys.OSFS{},
		Provider:                  provider,
		BDContext:                 preflightBDContextReader(cityPath),
		DatabaseProjectID:         preflightDatabaseProjectIDReader(cityPath),
		DeferIdentityToNativeOpen: preflightIdentityDeferredReader(cityPath),
	}
}

// preflightScopeIsRig reports whether scope is a rig subdirectory of the
// city (needing the rig-aware bd runner) rather than the city root itself.
// Comparison MUST resolve symlinks: on macOS in particular, cityPath and a
// rig-declared scope can be the identical directory reached through
// different spellings (e.g. /tmp vs the OS's real /private/tmp), and a
// lexical-only compare (filepath.Clean) would treat that as two different
// scopes, silently downgrading a real city-scope preflight into a
// rig-scoped one (gcf-bym6). samePath resolves symlinks before comparing.
func preflightScopeIsRig(scope, cityPath string) bool {
	return !samePath(scope, cityPath)
}

// preflightBDCommandRunnerForRig is bdCommandRunnerForRig without the
// managed-dolt recovery side effects: a preflight probe must fail fast
// rather than start (or wait to recover) a managed Dolt server just to
// report context. Mirrors scopedBdStoreForCity / bdRuntimeEnvWithErrorNoRecovery's
// established no-recovery pattern for exactly this class of caller (gascity
// ga-cdmx6x), applied to the rig-scoped env resolver so a preflight bd
// context read never has the side effect of starting a database.
func preflightBDCommandRunnerForRig(cityPath string, cfg *config.City, rigPath string) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		env, err := bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, cfg, rigPath)
		if err != nil {
			return nil, err
		}
		ensureProjectedDoltEnvExplicit(env)
		return beadsExecCommandRunnerWithEnv(env)(dir, name, args...)
	}
}

func preflightBDContextReader(cityPath string) func(scope string) (contract.PreflightBDContext, error) {
	var cfg *config.City
	return func(scope string) (contract.PreflightBDContext, error) {
		// Context is scope-specific.  Using the city runner for a rig pins
		// BEADS_DIR to the rig while leaving the Dolt mode/database projection
		// on the city, so bd can report embedded (or the wrong database) even
		// when the rig metadata declares server mode.  Select the same
		// rig-aware environment used by the eventual fallback store, but
		// never the recovery-enabled runner: a preflight probe must not
		// start a managed Dolt server as a side effect.
		runner := bdCommandRunnerForCity(cityPath)
		if preflightScopeIsRig(scope, cityPath) {
			if cfg == nil {
				cfg, _ = loadCityConfig(cityPath, io.Discard)
			}
			runner = preflightBDCommandRunnerForRig(cityPath, cfg, scope)
		}
		out, err := runner(scope, "bd", "context", "--json")
		if err != nil {
			return contract.PreflightBDContext{}, err
		}
		var raw struct {
			Backend       string `json:"backend"`
			DoltMode      string `json:"dolt_mode"`
			BDVersion     string `json:"bd_version"`
			SchemaVersion int    `json:"schema_version"`
		}
		if err := json.Unmarshal(out, &raw); err != nil {
			return contract.PreflightBDContext{}, fmt.Errorf("parse bd context --json: %w", err)
		}
		return contract.PreflightBDContext{
			Backend:       raw.Backend,
			DoltMode:      raw.DoltMode,
			BDVersion:     raw.BDVersion,
			SchemaVersion: raw.SchemaVersion,
		}, nil
	}
}

// preflightIdentityDeferredReader reports whether a scope resolves to an
// external Dolt endpoint (e.g. a hosted beads-gateway). The direct root/plaintext
// project_id probe cannot authenticate such endpoints, so when it comes back
// unconfirmed the identity check defers to beadslib's native-open verification
// (which authenticates via the credential command and refuses to connect on a
// _project_id mismatch) instead of degrading the scope off the native store.
func preflightIdentityDeferredReader(cityPath string) func(scope string) bool {
	return func(scope string) bool {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return false
		}
		return target.External
	}
}

func preflightDatabaseProjectIDReader(cityPath string) func(scope string) (string, bool, error) {
	return func(scope string) (string, bool, error) {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return "", false, err
		}
		db, err := managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
		if err != nil {
			return "", false, err
		}
		defer db.Close() //nolint:errcheck // read-only best-effort close

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return "", false, err
		}
		return readDatabaseProjectID(ctx, db)
	}
}
