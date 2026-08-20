package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
)

// This file implements `gc hook --claim --wait`: a zero-token event-driven wake
// watcher for the worker poll/drain loop (gp-7rch.16, kunchenguid roadmap #2).
//
// Today a worker that finds no claimable work runs an in-context bash poll loop
// (`for i in 1..6; do gc hook --claim; sleep 10; done`) before draining. Every
// iteration is a separate provider tool call, so the idle wait costs model
// tokens fleet-wide. `--wait` collapses that loop into ONE blocking CLI call
// that spends ZERO model tokens: it composes the existing claim protocol with
// the existing `gc events --watch` streaming primitive. It tries to claim
// immediately; if there is nothing to claim it blocks on the event stream until
// a candidate wake event arrives, then re-probes; on the overall timeout it
// returns the normal drain result (acknowledging runtime drain when the caller
// passed --drain-ack).
//
// Positive-evidence / fail-open: the "provably working" signal is a live event
// stream. If the streaming city API is unavailable, the watcher must not
// silently swallow a real stop — it FAILS OPEN to the historical sleep-poll
// cadence so behavior degrades to today's loop rather than hanging or draining
// prematurely.
//
// Wedge counter: if candidate events keep firing but no work is ever claimable
// across WedgeThreshold consecutive cycles, the watcher emits session.wedged and
// resets the counter, so a stuck queue or routing mismatch escalates instead of
// spinning invisibly.

const (
	// defaultHookWaitTimeout is the overall wake budget for a bare `--wait`
	// (no explicit duration). It matches the historical 6×10s poll-before-drain
	// window so the drain cadence is unchanged when no work ever arrives.
	defaultHookWaitTimeout = 60 * time.Second
	// defaultHookWaitIteration bounds a single blocking events-watch so the
	// watcher periodically re-probes the claim path even on a silent bus (a
	// candidate may have become claimable via a central wake computation that
	// did not emit an event this session subscribes to).
	defaultHookWaitIteration = 20 * time.Second
	// defaultHookWaitMinProbe floors the interval between claim re-probes after
	// an event fires, so a busy event bus cannot drive a tight work-query loop.
	defaultHookWaitMinProbe = 2 * time.Second
	// defaultHookWaitWedgeThreshold is how many consecutive event-fired-but-
	// nothing-claimable cycles trip a session.wedged escalation.
	defaultHookWaitWedgeThreshold = 5
)

// hookWaitConfig parameterizes the wake loop. CallerDrainAck / CallerJSON mirror
// the flags the operator passed so the FINAL (timeout) result is rendered in the
// caller's requested shape.
type hookWaitConfig struct {
	Timeout          time.Duration
	IterationTimeout time.Duration
	MinProbeInterval time.Duration
	WedgeThreshold   int
	CallerDrainAck   bool
	CallerJSON       bool
}

func (c *hookWaitConfig) applyDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = defaultHookWaitTimeout
	}
	if c.IterationTimeout <= 0 {
		c.IterationTimeout = defaultHookWaitIteration
	}
	if c.MinProbeInterval <= 0 {
		c.MinProbeInterval = defaultHookWaitMinProbe
	}
	if c.WedgeThreshold <= 0 {
		c.WedgeThreshold = defaultHookWaitWedgeThreshold
	}
}

// hookWaitEventResult reports the outcome of one blocking wait on the event
// stream. Available=false means the stream could not be used (API down, local
// -only scope, setup error): the caller fails open to a sleep. Fired=true means
// a candidate wake event arrived before the per-iteration timeout.
type hookWaitEventResult struct {
	Fired     bool
	Available bool
}

// hookWaitOps are the injectable seams for the wake loop. Production wires Probe
// to the federated claim path and WaitForEvent to the events-watch stream; tests
// substitute deterministic fakes.
type hookWaitOps struct {
	// Probe attempts exactly one claim against the federated stores. jsonMode
	// selects the output rendering written to the returned buffer; action is the
	// parsed protocol action ("work", "drain", or "" when unknown). The bead is
	// really claimed here on a "work" result — Probe has the same side effects as
	// a plain `gc hook --claim`.
	Probe func(drainAck, jsonMode bool) (action string, code int, out []byte)
	// WaitForEvent blocks up to timeout for a candidate wake event.
	WaitForEvent func(timeout time.Duration) hookWaitEventResult
	// EmitWedged escalates a stuck watcher (optional; nil disables escalation).
	EmitWedged func(cycles int)
	Now        func() time.Time
	Sleep      func(d time.Duration)
}

func (ops *hookWaitOps) applyDefaults() {
	if ops.Now == nil {
		ops.Now = time.Now
	}
	if ops.Sleep == nil {
		ops.Sleep = time.Sleep
	}
}

// runHookClaimWait is the wake-watcher state machine. It returns the process
// exit code and writes exactly one protocol result (a claim or a drain) to
// stdout, matching the single-result contract of plain `gc hook --claim`.
func runHookClaimWait(cfg hookWaitConfig, ops hookWaitOps, stdout, stderr io.Writer) int {
	cfg.applyDefaults()
	ops.applyDefaults()

	deadline := ops.Now().Add(cfg.Timeout)
	wedge := 0

	for {
		// Intermediate probe always renders JSON so we can read the protocol
		// action without scraping human output. The claim's real side effects
		// (branch stamp, continuation preassign) happen here on a work hit.
		action, code, out := ops.Probe(false, true)
		if action == hookWaitActionWork {
			return writeHookWaitWork(out, cfg.CallerJSON, code, stdout, stderr)
		}

		now := ops.Now()
		if !now.Before(deadline) {
			break // budget exhausted → fall through to the final drain
		}
		remaining := deadline.Sub(now)
		watchTimeout := cfg.IterationTimeout
		if remaining < watchTimeout {
			watchTimeout = remaining
		}

		res := ops.WaitForEvent(watchTimeout)
		switch {
		case !res.Available:
			// Fail open: the stream is unusable, so degrade to the historical
			// sleep-poll cadence rather than swallowing a real stop or hanging.
			ops.Sleep(watchTimeout)
			wedge = 0
		case res.Fired:
			// Activity arrived but the next probe may still find nothing
			// claimable. Count consecutive absorbed-but-stuck cycles; escalate
			// past the threshold, then floor the re-probe so a chatty bus cannot
			// spin the work query.
			wedge++
			if wedge >= cfg.WedgeThreshold {
				if ops.EmitWedged != nil {
					ops.EmitWedged(wedge)
				}
				wedge = 0
			}
			ops.Sleep(cfg.MinProbeInterval)
		default:
			// Per-iteration timeout with no event: a genuinely quiet bus, not a
			// wedge. Reset the counter and re-probe (a central wake computation
			// may have made work claimable without an event this session sees).
			wedge = 0
		}
	}

	// Budget exhausted. Run one final claim in the caller's requested shape,
	// honoring --drain-ack so this single call fully replaces the old
	// poll-then-drain-ack sequence.
	_, code, out := ops.Probe(cfg.CallerDrainAck, cfg.CallerJSON)
	_, _ = stdout.Write(out) //nolint:errcheck // best-effort passthrough
	return code
}

const hookWaitActionWork = "work"

// writeHookWaitWork renders a work hit discovered by an intermediate (always
// JSON) probe in the caller's requested shape: the raw JSON line when the caller
// asked for --json, else the bare bead ID that plain `gc hook --claim` prints.
func writeHookWaitWork(jsonOut []byte, callerJSON bool, code int, stdout, stderr io.Writer) int {
	if callerJSON {
		_, _ = stdout.Write(jsonOut) //nolint:errcheck
		return code
	}
	if beadID := parseHookClaimBeadID(jsonOut); beadID != "" {
		fmt.Fprintln(stdout, beadID) //nolint:errcheck
	}
	return code
}

// parseHookClaimAction extracts the protocol action from a JSON claim result
// line. Returns "" when the bytes are not a decodable result (e.g. an empty
// non-JSON drain), which the caller treats as not-work.
func parseHookClaimAction(jsonLine []byte) string {
	var r hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(jsonLine), &r); err != nil {
		return ""
	}
	return strings.TrimSpace(r.Action)
}

func parseHookClaimBeadID(jsonLine []byte) string {
	var r hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(jsonLine), &r); err != nil {
		return ""
	}
	return strings.TrimSpace(r.BeadID)
}

// hookWaitForCandidateEvent blocks up to timeout on the city event stream for
// the next candidate wake event, reusing the same streaming primitive as
// `gc events --watch`. An empty type filter subscribes to every event: because
// claimability is a computed predicate with no single "bead became claimable for
// agent X" event, any activity is a cheap signal to re-probe. Returns
// Available=false whenever the stream cannot be used, so the caller fails open.
func hookWaitForCandidateEvent(timeout time.Duration, stderr io.Writer) hookWaitEventResult {
	scope, err := resolveEventsScope("")
	if err != nil || scope.localOnly {
		return hookWaitEventResult{Available: false}
	}
	var buf bytes.Buffer
	// Discard the watch's own stderr: a fail-open path should stay quiet rather
	// than spam the worker's log with streaming-API-required notices every cycle.
	code := doEventsWatch(scope, "", nil, 0, "", timeout, &buf, io.Discard)
	if code != 0 {
		return hookWaitEventResult{Available: false}
	}
	// doEventsWatch returns 0 both on a match (a JSON line printed) and on a
	// clean per-iteration timeout (nothing printed). Non-empty stdout ⇒ fired.
	return hookWaitEventResult{Available: true, Fired: strings.TrimSpace(buf.String()) != ""}
}

// emitCitySessionWedged records a session.wedged escalation on the city event
// log. Best-effort and correlated by the current session ID; a missing session
// ID drops the emission (nothing to correlate).
func emitCitySessionWedged(cityPath string, stderr io.Writer, sessionID, template string, cycles int) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	rec := openCityRecorderAt(cityPath, stderr)
	if closer, ok := rec.(interface{ Close() error }); ok {
		defer closer.Close() //nolint:errcheck
	}
	template = strings.TrimSpace(template)
	subject := template
	if subject == "" {
		subject = sessionID
	}
	reason := fmt.Sprintf("wake watcher absorbed %d candidate wake cycles without claimable work", cycles)
	rec.Record(events.Event{
		Type:    events.SessionWedged,
		Actor:   eventActor(),
		Subject: subject,
		Message: reason,
		Payload: api.SessionLifecyclePayloadJSON(sessionID, template, reason),
	})
}
