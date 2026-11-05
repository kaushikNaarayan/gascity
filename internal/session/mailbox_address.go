package session

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file is the session-class READ half of the mailbox-address codec
// consumed by the mail CLI/API. Resolving the address a session publishes mail
// under is a session-attribute read, not a mail op (mail itself is already
// front-doored via mail.Provider). Confining the codec here keeps the
// session-bead metadata vocabulary (alias / alias_history / session_name) out
// of cmd/gc and internal/api, so the mail callers speak session addresses
// instead of cracking beads.Bead.Metadata directly.

// MailboxAddress returns the primary mailbox address a session bead publishes
// under: its alias if set, else its bead id, else its session_name. It is the
// pure, side-effect-free codec for a single session bead — the canonical home
// of the logic the mail CLI previously inlined as sessionMailboxAddress.
func MailboxAddress(b beads.Bead) string {
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
		return alias
	}
	if b.ID != "" {
		return b.ID
	}
	return strings.TrimSpace(b.Metadata["session_name"])
}

// MailboxAddresses returns every address a session bead can receive mail at —
// its primary address, its bead id, any retained alias history, and its runtime
// session_name (appended last) — deduped and trimmed. It is the single
// recipient-set rule shared by the mail CLI and the API inbox/count reads, so
// mail persisted under a runtime session_name is visible to both
// (guarded by TestMailAPIQueriesAllResolvedSessionMailboxAddresses).
func MailboxAddresses(b beads.Bead) []string {
	seen := map[string]bool{}
	var addresses []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		addresses = append(addresses, value)
	}
	add(MailboxAddress(b))
	add(b.ID)
	for _, alias := range AliasHistory(b.Metadata) {
		add(alias)
	}
	add(b.Metadata["session_name"])
	return addresses
}

// MailboxAddressFromInfo is the Info-taking twin of MailboxAddress: the primary
// mailbox address a session publishes under — its alias if set, else its bead
// id, else its runtime session_name. It reads Info fields that mirror the exact
// bead metadata (Alias, ID, SessionNameMetadata — the RAW session_name without
// the sessionNameFor fallback), so it is byte-identical to MailboxAddress.
func MailboxAddressFromInfo(info Info) string {
	if alias := strings.TrimSpace(info.Alias); alias != "" {
		return alias
	}
	if info.ID != "" {
		return info.ID
	}
	return strings.TrimSpace(info.SessionNameMetadata)
}

// MailboxAddressesFromInfo is the Info-taking twin of MailboxAddresses: every
// address a session can receive mail at (primary, bead id, alias history, and
// the RAW SessionNameMetadata appended last), byte-identical to
// MailboxAddresses.
func MailboxAddressesFromInfo(info Info) []string {
	seen := map[string]bool{}
	var addresses []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		addresses = append(addresses, value)
	}
	add(MailboxAddressFromInfo(info))
	add(info.ID)
	for _, alias := range info.AliasHistory {
		add(alias)
	}
	add(info.SessionNameMetadata)
	return addresses
}

// ExtmsgHandleSource returns the raw handle source for a session bead used by
// the external-messaging handle projection: alias if set, else session_name.
// Unlike MailboxAddress it does NOT fall back to the bead id — it preserves the
// exact precedence the extmsg handler relied on before routing through the
// session front door. Callers still apply their own handle-label trimming.
func ExtmsgHandleSource(b beads.Bead) string {
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
		return alias
	}
	return strings.TrimSpace(b.Metadata["session_name"])
}

// MailboxAddress loads the session bead for id and returns its primary mailbox
// address. It confines the store.Get + codec behind the typed read seam so mail
// callers stop calling store.Get(id) and reading b.Metadata themselves. The Get
// error is returned verbatim (beads.ErrNotFound-wrapped when the bead is
// absent), matching the raw mail path it replaces — the callers pass an
// already-resolved session id and surface the error to the operator.
func (s *Store) MailboxAddress(id string) (string, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return "", err
	}
	return MailboxAddress(b), nil
}

// MailboxAddresses loads the session bead for id and returns all addresses it
// can receive mail at. The Get error is returned verbatim, matching the raw
// mail path it replaces.
func (s *Store) MailboxAddresses(id string) ([]string, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	return MailboxAddresses(b), nil
}

// ExtmsgHandleSource loads the session bead for id and returns its extmsg
// handle source (alias, else session_name; no bead-id fallback). The (source,
// false) return signals "no session bead / load error" so the caller can fall
// back to its selector, matching the raw extmsg handler which fell back on any
// Get error.
func (s *Store) ExtmsgHandleSource(id string) (string, bool) {
	b, err := s.store.Get(id)
	if err != nil {
		return "", false
	}
	return ExtmsgHandleSource(b), true
}
