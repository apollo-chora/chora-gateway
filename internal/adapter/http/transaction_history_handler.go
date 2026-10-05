// transaction_history_handler.go — chora-gateway REST surface for the ADR-205
// contextual Transaction History (CHO-1940 / Wave B4). Proxies
// openapi/transaction-history.yaml to chora-tenancy's TransactionHistoryService
// gRPC (via clients.TransactionHistoryClient), resolving the explicit ADR-205
// D1 scope per route + stamping the caller's mesh identity on the outbound gRPC
// call (the client does the metadata stamp).
//
// Scope per route (D1):
//   - /api/v1/me/transactions*    → LEARNER  (own GCID + active tenant)
//   - /api/v1/admin/transactions* → TENANT, or MASTER when the caller holds the
//     platform_operator role (case-insensitive). MASTER span-all + franchisee
//     selection + the IMDA-D1 audit are enforced server-side in chora-tenancy;
//     this handler only sets the scope + forwards the validated filters.
//
// list / summary / {ledger_id} (detail) are served here. /export(+jobs) and
// /stream (SSE) are Wave B5/CHO-1941 — they return 501 (fail-loud, not faked).
//
// JWT gate: the two prefixes are in DefaultJWTGatedPrefixes so
// RequireChoraSessionJWT stamps MeshClaims before this handler runs.
package httpadapter

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

const (
	txLearnerPrefix = "/api/v1/me/transactions"
	txAdminPrefix   = "/api/v1/admin/transactions"
)

// TxHistoryClient is the slice of clients.TransactionHistoryClient this handler
// depends on (interface for test injection).
type TxHistoryClient interface {
	List(ctx context.Context, caller clients.Caller, p clients.TxListParams) (clients.TxListResponseDTO, error)
	Detail(ctx context.Context, caller clients.Caller, scope clients.TxScope, ledgerID, pageToken string, pageSize int32) (clients.TxDetailResponseDTO, error)
	Summary(ctx context.Context, caller clients.Caller, p clients.TxListParams) (clients.TxSummaryDTO, error)
	CreateExport(ctx context.Context, caller clients.Caller, p clients.TxListParams, format string) (clients.TxExportJobDTO, error)
	GetExport(ctx context.Context, caller clients.Caller, scope clients.TxScope, jobID string) (clients.TxExportJobDTO, error)
	ListFranchisees(ctx context.Context, caller clients.Caller, q string, pageSize int32, pageToken string) (clients.TxFranchiseesDTO, error)
}

// validExportFormats are the OpenAPI ExportFormatQuery values.
var validExportFormats = map[string]bool{"csv": true, "json": true}

// TransactionHistoryHandler serves the /me + /admin transaction routes.
type TransactionHistoryHandler struct {
	client TxHistoryClient
}

// NewTransactionHistoryHandler fails loud on a nil client.
func NewTransactionHistoryHandler(client TxHistoryClient) (*TransactionHistoryHandler, error) {
	if client == nil {
		return nil, errors.New("httpadapter: transaction history client required")
	}
	return &TransactionHistoryHandler{client: client}, nil
}

// WithTransactionHistory composes the handler with a base handler: paths under
// the two transaction prefixes are served here; everything else falls through.
// Passes through when h is nil (env-driven dev opt-out).
func WithTransactionHistory(base http.Handler, h *TransactionHistoryHandler) http.Handler {
	if h == nil {
		return base
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case underPrefix(r.URL.Path, txLearnerPrefix):
			h.serve(w, r, clients.TxScopeLearner, txLearnerPrefix, nil)
		case underPrefix(r.URL.Path, txAdminPrefix):
			h.serve(w, r, scopeForAdmin(r), txAdminPrefix, sessionRoles(r))
		default:
			base.ServeHTTP(w, r)
		}
	})
}

// underPrefix matches the exact prefix OR a subtree under it.
func underPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// scopeForAdmin returns MASTER for a platform_operator caller, else TENANT.
func scopeForAdmin(r *http.Request) clients.TxScope {
	if hasPlatformOperatorRole(r) {
		return clients.TxScopeMaster
	}
	return clients.TxScopeTenant
}

// serve dispatches the sub-route under a prefix to list / summary / detail,
// or 501 for the not-yet-built export/stream (B5).
func (h *TransactionHistoryHandler) serve(w http.ResponseWriter, r *http.Request, scope clients.TxScope, prefix string, roles []string) {
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case tail == "" || tail == "/":
		h.handleList(w, r, scope, roles)
	case tail == "/summary":
		h.handleSummary(w, r, scope, roles)
	case tail == "/stream":
		writeError(w, http.StatusNotImplemented, "GATEWAY_NOT_IMPLEMENTED", "transaction stream (SSE live tail) is served by a dedicated long-lived route, not this BFF mux")
	case tail == "/export":
		h.handleExportCreate(w, r, scope, roles)
	case strings.HasPrefix(tail, "/export/jobs/"):
		jobID := strings.TrimPrefix(tail, "/export/jobs/")
		if jobID == "" || strings.Contains(jobID, "/") {
			writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND", "no such export job route")
			return
		}
		h.handleExportPoll(w, r, scope, roles, jobID)
	case strings.HasPrefix(tail, "/export/"):
		writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND", "no such export route")
	case tail == "/franchisees":
		h.handleFranchisees(w, r, scope, roles)
	default:
		// /{ledger_id} — reject any deeper path.
		ledgerID := strings.TrimPrefix(tail, "/")
		if ledgerID == "" || strings.Contains(ledgerID, "/") {
			writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND", "no such transaction route")
			return
		}
		h.handleDetail(w, r, scope, roles, ledgerID)
	}
}

func (h *TransactionHistoryHandler) handleList(w http.ResponseWriter, r *http.Request, scope clients.TxScope, roles []string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	caller, ok := h.caller(w, r, scope, roles)
	if !ok {
		return
	}
	params, ok := parseListParams(w, r, scope)
	if !ok {
		return
	}
	resp, err := h.client.List(r.Context(), caller, params)
	if err != nil {
		writeTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *TransactionHistoryHandler) handleSummary(w http.ResponseWriter, r *http.Request, scope clients.TxScope, roles []string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	caller, ok := h.caller(w, r, scope, roles)
	if !ok {
		return
	}
	params, ok := parseListParams(w, r, scope)
	if !ok {
		return
	}
	resp, err := h.client.Summary(r.Context(), caller, params)
	if err != nil {
		writeTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleFranchisees serves the master's franchisee directory for the filter
// picker: GET /api/v1/admin/transactions/franchisees?q=&page_size=&page_token=.
// MASTER (platform_operator) scope ONLY — a tenant admin / learner cannot list
// cross-tenant franchisees.
func (h *TransactionHistoryHandler) handleFranchisees(w http.ResponseWriter, r *http.Request, scope clients.TxScope, roles []string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if scope != clients.TxScopeMaster {
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN", "listing franchisees requires the platform_operator role")
		return
	}
	caller, ok := h.caller(w, r, scope, roles)
	if !ok {
		return
	}
	pageSize, ok := parsePageSize(w, r)
	if !ok {
		return
	}
	resp, err := h.client.ListFranchisees(
		r.Context(), caller,
		strings.TrimSpace(r.URL.Query().Get("q")),
		pageSize,
		strings.TrimSpace(r.URL.Query().Get("page_token")),
	)
	if err != nil {
		writeTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *TransactionHistoryHandler) handleDetail(w http.ResponseWriter, r *http.Request, scope clients.TxScope, roles []string, ledgerID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	caller, ok := h.caller(w, r, scope, roles)
	if !ok {
		return
	}
	pageSize, ok := parsePageSize(w, r)
	if !ok {
		return
	}
	resp, err := h.client.Detail(r.Context(), caller, scope, ledgerID, strings.TrimSpace(r.URL.Query().Get("page_token")), pageSize)
	if err != nil {
		writeTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleExportCreate enqueues an async export (202 + ExportJob handle). GET
// with the same filters as list + a required `format` (csv|json) query param.
func (h *TransactionHistoryHandler) handleExportCreate(w http.ResponseWriter, r *http.Request, scope clients.TxScope, roles []string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	format := strings.TrimSpace(r.URL.Query().Get("format"))
	if !validExportFormats[format] {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "format must be csv or json")
		return
	}
	caller, ok := h.caller(w, r, scope, roles)
	if !ok {
		return
	}
	params, ok := parseListParams(w, r, scope)
	if !ok {
		return
	}
	job, err := h.client.CreateExport(r.Context(), caller, params, format)
	if err != nil {
		writeTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// handleExportPoll returns one export job's status + (when ready) signed URL.
func (h *TransactionHistoryHandler) handleExportPoll(w http.ResponseWriter, r *http.Request, scope clients.TxScope, roles []string, jobID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	caller, ok := h.caller(w, r, scope, roles)
	if !ok {
		return
	}
	job, err := h.client.GetExport(r.Context(), caller, scope, jobID)
	if err != nil {
		writeTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// caller resolves the mesh identity + applies the per-scope requirement:
//   - LEARNER / TENANT need gcid + tenant_id (own data).
//   - MASTER (operator) needs gcid only (span-all uses the platform sentinel
//     server-side; the operator may carry no tenant membership, ADR-165).
//
// Writes the 401 + returns ok=false when the requirement is unmet.
func (h *TransactionHistoryHandler) caller(w http.ResponseWriter, r *http.Request, scope clients.TxScope, roles []string) (clients.Caller, bool) {
	gcid, tenant := txIdentity(r)
	if gcid == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", "gcid missing from session")
		return clients.Caller{}, false
	}
	if scope != clients.TxScopeMaster && tenant == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", "tenant_id missing from session")
		return clients.Caller{}, false
	}
	return clients.Caller{GCID: gcid, TenantID: tenant, Roles: roles}, true
}

// txIdentity reads gcid + tenant_id from the validated MeshClaims (fallback to
// the raw ChoraSession claims, mirroring requireMeshIdentity).
func txIdentity(r *http.Request) (gcid, tenant string) {
	if mc, ok := MeshClaimsFromContext(r.Context()); ok && mc != nil {
		gcid = strings.TrimSpace(mc.GCID)
		tenant = strings.TrimSpace(mc.TenantID)
	}
	if gcid == "" || tenant == "" {
		if cs, ok := ChoraSessionClaimsFromContext(r.Context()); ok && cs != nil {
			if gcid == "" {
				gcid = strings.TrimSpace(cs.GCID)
			}
			if tenant == "" {
				tenant = strings.TrimSpace(cs.TenantID)
			}
		}
	}
	return gcid, tenant
}

// -----------------------------------------------------------------------------
// query param parse + validate (400 on invalid)
// -----------------------------------------------------------------------------

var (
	validKinds    = map[string]bool{"purchase": true, "mana_topup": true, "mana_spend_daily": true}
	validStatuses = map[string]bool{"captured": true, "refunded": true, "failed": true, "expired": true, "posted": true, "all": true}
	validSorts    = map[string]bool{"occurred_at:desc": true, "occurred_at:asc": true, "amount:desc": true, "amount:asc": true}
	validPageSize = map[int]bool{10: true, 20: true, 50: true, 100: true}
)

func parseListParams(w http.ResponseWriter, r *http.Request, scope clients.TxScope) (clients.TxListParams, bool) {
	q := r.URL.Query()
	p := clients.TxListParams{Scope: scope, PageToken: strings.TrimSpace(q.Get("page_token"))}

	if kind := strings.TrimSpace(q.Get("kind")); kind != "" {
		if !validKinds[kind] {
			writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "invalid kind")
			return clients.TxListParams{}, false
		}
		p.Kind = kind
	}
	if st := strings.TrimSpace(q.Get("status")); st != "" {
		if !validStatuses[st] {
			writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "invalid status")
			return clients.TxListParams{}, false
		}
		if st != "all" { // "all" = no status filter
			p.Status = st
		}
	}
	if sort := strings.TrimSpace(q.Get("sort")); sort != "" {
		if !validSorts[sort] {
			writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "invalid sort")
			return clients.TxListParams{}, false
		}
		p.Sort = sort
	}
	if from, ok := parseRFC3339Param(w, q.Get("from"), "from"); !ok {
		return clients.TxListParams{}, false
	} else if from != nil {
		p.From = from
	}
	if to, ok := parseRFC3339Param(w, q.Get("to"), "to"); !ok {
		return clients.TxListParams{}, false
	} else if to != nil {
		p.To = to
	}
	ps, ok := parsePageSize(w, r)
	if !ok {
		return clients.TxListParams{}, false
	}
	p.PageSize = ps

	// Operator/admin-only selectors. Silently ignored server-side for other
	// scopes, but only forward them for admin routes to avoid leaking intent.
	// Both accept REPEATED query params (?managed_tenant_id=a&managed_tenant_id=b
	// / ?learner_gcid=a&learner_gcid=b) AND a single comma-joined value
	// (?...=a,b); each element must be a UUID (400 on a malformed element).
	if scope == clients.TxScopeTenant || scope == clients.TxScopeMaster {
		franchisees, ok := parseUUIDListParam(w, q["managed_tenant_id"], "managed_tenant_id")
		if !ok {
			return clients.TxListParams{}, false
		}
		p.ManagedTenantIDs = franchisees
		learners, ok := parseUUIDListParam(w, q["learner_gcid"], "learner_gcid")
		if !ok {
			return clients.TxListParams{}, false
		}
		p.LearnerGCIDs = learners
	}
	return p, true
}

// parseUUIDListParam flattens repeated query values (each of which may itself
// be a comma-joined list), trims + drops empties, and validates every element
// is a UUID. On any malformed element it writes a 400 and returns ok=false.
// An absent/empty param yields (nil, true) — no filter.
func parseUUIDListParam(w http.ResponseWriter, raw []string, name string) ([]string, bool) {
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !isUUIDShape(part) {
				writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", name+" must be a UUID")
				return nil, false
			}
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

// isUUIDShape is a cheap 8-4-4-4-12 hex-and-dash guard (mirrors the tenancy
// server's looksLikeUUID) — rejects sentinels + malformed ids before dispatch.
func isUUIDShape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func parsePageSize(w http.ResponseWriter, r *http.Request) (int32, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("page_size"))
	if raw == "" {
		return 0, true // server applies the default
	}
	// ParseInt with explicit 32-bit size: rejects out-of-range values instead
	// of wrapping (G109/G115 — no int→int32 overflow possible).
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || !validPageSize[int(n)] {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "page_size must be one of 10, 20, 50, 100")
		return 0, false
	}
	return int32(n), true
}

func parseRFC3339Param(w http.ResponseWriter, raw, name string) (*time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", name+" must be an RFC-3339 timestamp")
		return nil, false
	}
	tu := t.UTC()
	return &tu, true
}

// writeTxErr maps an upstream gRPC status to the canonical HTTP error envelope.
// (Mirrors writeCheckoutErr; named for this surface to keep the two decoupled.)
func writeTxErr(w http.ResponseWriter, err error) {
	st, ok := status.FromError(unwrap(err))
	if !ok {
		writeError(w, http.StatusInternalServerError, "GATEWAY_UPSTREAM_ERROR", err.Error())
		return
	}
	switch st.Code() {
	case codes.NotFound:
		writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND", st.Message())
	case codes.InvalidArgument:
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", st.Message())
	case codes.PermissionDenied:
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN", st.Message())
	case codes.Unauthenticated:
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", st.Message())
	case codes.Unavailable, codes.DeadlineExceeded:
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_UPSTREAM_UNAVAILABLE", st.Message())
	default:
		writeError(w, http.StatusInternalServerError, "GATEWAY_UPSTREAM_ERROR", st.Message())
	}
}
