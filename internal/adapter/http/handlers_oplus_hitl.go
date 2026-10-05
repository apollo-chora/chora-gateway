// handlers_oplus_hitl.go — N13 BFF passthrough for the HITL self-claim +
// release endpoints shipped by N7 (chora-governance commit `03e7fbc3`),
// extended with the approve + reject verdict endpoints.
//
// Routes (mounted under the /bff/oplus/ surface; auditor-role-gated by the
// outer AuditorGate middleware):
//
//	POST /bff/oplus/governance/hitl/{id}/claim    body {"operator_gcid":"..."}
//	POST /bff/oplus/governance/hitl/{id}/release  body {"operator_gcid":"..."}
//	POST /bff/oplus/governance/hitl/{id}/approve  body {"operator_gcid":"...","note?":"..."}
//	POST /bff/oplus/governance/hitl/{id}/reject   body {"operator_gcid":"...","note?":"..."}
//
// Status mapping (per N7 contract + FE-facing semantics):
//
//	200 — claim/release/approve/reject succeeded; body is the updated
//	      HITLDecisionItem
//	404 — decision_id not found upstream (cross-tenant ⇒ same — no leak)
//	409 — already claimed (claim) / terminal verdict (any action)
//	403 — release/approve/reject attempted by non-assignee
//	422 — invalid body (blank operator_gcid OR malformed JSON)
//	503 — upstream chora-governance unreachable OR client not wired
//
// HITL claim/release/approve/reject are WRITE actions — the BFF does NOT
// collapse them to the read-side OPlusEnvelope `state:'error'` 200 envelope.
// Errors flow out as real HTTP status codes so the FE can render targeted
// toast notifications + retry decisions.
package httpadapter

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// hitlActionRequest mirrors the chora-governance request body. The
// operator_gcid is the AUDITOR's own GCID (NOT a FE-picked parameter).
// Note is an optional free-text rationale accepted by the approve/reject
// verdict endpoints; claim/release ignore it.
type hitlActionRequest struct {
	OperatorGcid string `json:"operator_gcid"`
	Note         string `json:"note,omitempty"`
}

// hitlAction dispatches POST
// /bff/oplus/governance/hitl/{id}/{claim|release|approve|reject}.
// Registered by RegisterOPlusRoutes via the
// `/bff/oplus/governance/hitl/` prefix handler.
func (h *OPlusHandler) hitlAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only")
		return
	}

	decisionID, action, ok := parseHITLActionPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "GATEWAY_HITL_NOT_FOUND",
			"unknown hitl action (expected /bff/oplus/governance/hitl/{id}/{claim|release|approve|reject})")
		return
	}

	operatorGcid, note, ok := decodeHITLActionBody(w, r)
	if !ok {
		// decodeHITLActionBody already wrote the 422 response.
		return
	}

	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	if h.Governance == nil {
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_GOVERNANCE_UNAVAILABLE",
			"governance client not wired")
		return
	}

	var (
		item *upstream.HITLDecisionItem
		err  error
	)
	switch action {
	case "claim":
		item, err = h.Governance.ClaimHITLDecision(ctx, tenantID, decisionID, operatorGcid)
	case "release":
		item, err = h.Governance.ReleaseHITLDecision(ctx, tenantID, decisionID, operatorGcid)
	case "approve":
		item, err = h.Governance.ApproveHITLDecision(ctx, tenantID, decisionID, operatorGcid, note)
	case "reject":
		item, err = h.Governance.RejectHITLDecision(ctx, tenantID, decisionID, operatorGcid, note)
	default:
		writeError(w, http.StatusNotFound, "GATEWAY_HITL_NOT_FOUND",
			"unknown hitl action (expected claim, release, approve, or reject)")
		return
	}
	if err != nil {
		writeHITLError(w, action, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// parseHITLActionPath extracts ({id}, {action}, ok) from
// `/bff/oplus/governance/hitl/{id}/{action}` paths. Returns ok=false on any
// path shape mismatch (caller writes 404).
func parseHITLActionPath(p string) (string, string, bool) {
	const prefix = "/bff/oplus/governance/hitl/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	id := strings.TrimSpace(parts[0])
	action := strings.ToLower(strings.TrimSpace(parts[1]))
	if id == "" || action == "" {
		return "", "", false
	}
	return id, action, true
}

// decodeHITLActionBody parses the `{operator_gcid:"...", note?:"..."}` request
// body. operator_gcid is required; note is optional (accepted but only
// forwarded by the approve/reject verdict actions). DisallowUnknownFields
// still rejects any key other than operator_gcid + note. On any validation
// failure writes a 422 response and returns ok=false. On success returns the
// trimmed operator_gcid + trimmed note + ok=true.
func decodeHITLActionBody(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if r.Body == nil {
		writeError(w, http.StatusUnprocessableEntity, "GATEWAY_INVALID_BODY",
			"operator_gcid is required")
		return "", "", false
	}
	var req hitlActionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusUnprocessableEntity, "GATEWAY_INVALID_BODY",
				"operator_gcid is required")
			return "", "", false
		}
		writeError(w, http.StatusUnprocessableEntity, "GATEWAY_INVALID_BODY", err.Error())
		return "", "", false
	}
	trimmed := strings.TrimSpace(req.OperatorGcid)
	if trimmed == "" {
		writeError(w, http.StatusUnprocessableEntity, "GATEWAY_OPERATOR_GCID_REQUIRED",
			"operator_gcid is required (blank string is not a valid GCID)")
		return "", "", false
	}
	return trimmed, strings.TrimSpace(req.Note), true
}

// writeHITLError maps the upstream client's typed errors to FE-facing HTTP
// statuses per the contract. Unknown errors collapse to 503 since the BFF
// considers them upstream transport / availability failures.
func writeHITLError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, upstream.ErrHITLNotFound):
		writeError(w, http.StatusNotFound, "GATEWAY_HITL_NOT_FOUND",
			"hitl decision not found")
	case errors.Is(err, upstream.ErrHITLConflict):
		writeError(w, http.StatusConflict, "GATEWAY_HITL_CONFLICT",
			"hitl decision is already claimed or has a recorded verdict")
	case errors.Is(err, upstream.ErrHITLForbidden):
		writeError(w, http.StatusForbidden, "GATEWAY_HITL_NOT_ASSIGNEE",
			"only the current assignee may release this gate")
	case errors.Is(err, upstream.ErrHITLInvalid):
		writeError(w, http.StatusUnprocessableEntity, "GATEWAY_HITL_INVALID",
			"hitl decision validation failed")
	default:
		log.Printf("oplus hitl %s upstream error: %v", action, err)
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_GOVERNANCE_UNAVAILABLE",
			"chora-governance is unavailable")
	}
}
