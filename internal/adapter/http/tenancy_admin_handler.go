// tenancy_admin_handler.go — chora-gateway operator-gated REST surface for
// ADR-217 Phase 2.2 (CHO-2008): franchise sub-tenant creation. Proxies
// POST /api/v1/tenancy/sub-tenants to chora-tenancy's Tenancy/CreateSubTenant
// gRPC (via clients.TenancyAdminClient), which persists the sub-tenant under an
// existing parent (parent_tenant_id + hosting_mode) in the chora-master tree.
//
// AUTHZ (critical): the chora-tenancy server does NO app-layer role check — it
// trusts the Istio AuthorizationPolicy allow-list + THIS gateway gate. So the
// platform_operator gate below is the SOLE application-layer authorization for
// the sub-tenant create-path. A non-operator MUST be refused here (403) before
// any outbound gRPC call.
//
// JWT gate: /api/v1/tenancy/sub-tenants is in DefaultJWTGatedPrefixes so
// RequireChoraSessionJWT stamps MeshClaims (incl. Roles) before this handler runs.
// Additive surface: an unset CHORA_TENANCY_GRPC_ADDR degrades (route 404) rather
// than bricking the gateway — see cmd/server/tenancy_admin_loader.go.
package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

// subTenantsPath is the sub-tenant create route (Phase 2.2). hierarchyPath is
// the direct-children read route (Phase 2.3b, CHO-2010). Both are owned by this
// handler; anything else under /api/v1/tenancy/* falls through.
const (
	subTenantsPath = "/api/v1/tenancy/sub-tenants"
	hierarchyPath  = "/api/v1/tenancy/tenants/current/hierarchy"
)

// maxSubTenantBodyBytes caps the request body (defence-in-depth; the body has
// four short fields plus an optional add-on code list).
const maxSubTenantBodyBytes = 1 << 20

// Shape bounds for add_on_codes (E1). The gateway validates SHAPE only:
// membership in the catalogue is chora-tenancy's to decide, because the
// catalogue is data seeded by migration 0035 and reconciled at tenancy boot,
// so a list here would be inline config that drifts the first time an add-on
// is added. validHostingModes is not a precedent for hard-coding one: a
// hosting mode is a closed proto enum, an add-on is a row.
const (
	maxAddOnCodeLen   = 64
	maxAddOnCodeCount = 64
)

// validHostingModes are the OpenAPI HostingMode enum values (tenancy-admin.yaml).
var validHostingModes = map[string]bool{
	"PLATFORM_HOSTED": true,
	"WHITE_LABEL":     true,
	"FRANCHISE":       true,
	"SELF_HOST":       true,
}

// SubTenantCreator is the create-path slice of clients.TenancyAdminClient
// (Phase 2.2).
type SubTenantCreator interface {
	CreateSubTenant(ctx context.Context, caller clients.Caller, p clients.SubTenantParams) (clients.TenantDTO, error)
}

// HierarchyGetter is the read-path slice of clients.TenancyAdminClient
// (Phase 2.3b) — direct children of the caller's current tenant + counts.
type HierarchyGetter interface {
	GetTenantHierarchy(ctx context.Context, caller clients.Caller, tenantID string) (clients.TenantHierarchyDTO, error)
}

// TenancyAdminClient is the combined port this handler depends on (both
// tenancy-admin routes). *clients.TenancyAdminClient satisfies it.
type TenancyAdminClient interface {
	SubTenantCreator
	HierarchyGetter
}

// TenancyAdminHandler serves the operator sub-tenant create route + the
// operator/tenant-admin hierarchy read route.
type TenancyAdminHandler struct {
	client TenancyAdminClient
}

// NewTenancyAdminHandler fails loud on a nil client.
func NewTenancyAdminHandler(client TenancyAdminClient) (*TenancyAdminHandler, error) {
	if client == nil {
		return nil, errors.New("httpadapter: tenancy admin client required")
	}
	return &TenancyAdminHandler{client: client}, nil
}

// WithTenancyAdmin composes the handler with a base handler: the single
// sub-tenants path is served here; everything else (incl. other /api/v1/tenancy/*
// paths) falls through. Passes through when h is nil (env-driven dev opt-out).
func WithTenancyAdmin(base http.Handler, h *TenancyAdminHandler) http.Handler {
	if h == nil {
		return base
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case subTenantsPath:
			h.handleCreateSubTenant(w, r)
			return
		case hierarchyPath:
			h.handleGetHierarchy(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// createSubTenantBody is the POST /api/v1/tenancy/sub-tenants request body
// (matches openapi/tenancy-admin.yaml CreateSubTenantRequest).
type createSubTenantBody struct {
	ParentTenantID string   `json:"parent_tenant_id"`
	DisplayName    string   `json:"display_name"`
	HostingMode    string   `json:"hosting_mode"`
	OwnerGCID      string   `json:"owner_gcid"`
	AddOnCodes     []string `json:"add_on_codes"`
}

// handleCreateSubTenant: POST only. Order = authenticate (401) -> authorize
// operator (403) -> validate body (400) -> proxy -> 201. The operator gate runs
// BEFORE the body is decoded so a non-operator can never trigger a gRPC call.
func (h *TenancyAdminHandler) handleCreateSubTenant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
		return
	}

	gcid, tenant := txIdentity(r)
	if gcid == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", "gcid missing from session")
		return
	}
	if !hasPlatformOperatorRole(r) {
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN", "creating a sub-tenant requires the platform_operator role")
		return
	}

	body, ok := decodeSubTenantBody(w, r)
	if !ok {
		return
	}
	params, ok := validateSubTenant(w, body)
	if !ok {
		return
	}

	dto, err := h.client.CreateSubTenant(r.Context(),
		clients.Caller{GCID: gcid, TenantID: tenant, Roles: sessionRoles(r)}, params)
	if err != nil {
		writeSubTenantErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, dto)
}

// handleGetHierarchy: GET only. Returns the DIRECT children of the caller's
// CURRENT tenant (resolved from the validated session — never a client-supplied
// id, so no cross-tenant enumeration) with per-child user counts. Order =
// authenticate (401) -> require current tenant (401) -> authorize operator OR
// tenant-admin (403) -> proxy -> 200.
//
// AUTHZ (critical): like the sub-tenant create route, the chora-tenancy server
// does NO app-layer role check — THIS gate is the sole application-layer authz.
// The operator/tenant-admin gate is REQUIRED: ADR-182 auto-enrols new learners
// into the public chora-master tenant, so an ungated read would let any learner
// enumerate chora-master's franchises + user counts.
func (h *TenancyAdminHandler) handleGetHierarchy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}

	gcid, tenant := txIdentity(r)
	if gcid == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", "gcid missing from session")
		return
	}
	if tenant == "" {
		// "current" tenant is unresolvable (e.g. an operator with no active
		// tenant) — the recursive subtree view lives on /admin/transactions/franchisees.
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", "current tenant_id missing from session")
		return
	}
	if !hasPlatformOperatorRole(r) && !hasTenantAdminRole(r) {
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN", "reading tenant hierarchy requires the platform_operator or tenant_admin role")
		return
	}

	dto, err := h.client.GetTenantHierarchy(r.Context(),
		clients.Caller{GCID: gcid, TenantID: tenant, Roles: sessionRoles(r)}, tenant)
	if err != nil {
		writeSubTenantErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// hasTenantAdminRole reports whether the session carries a tenant-admin-tier
// role (tenant_admin / admin / owner / super_admin; '-'/'_' interchangeable,
// case-insensitive — mirrors the role→hplus surfaces map in chora-tenancy and
// hasPlatformOperatorRole's liberal-in posture).
func hasTenantAdminRole(r *http.Request) bool {
	for _, role := range sessionRoles(r) {
		switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(role), "-", "_")) {
		case "tenant_admin", "admin", "owner", "super_admin":
			return true
		}
	}
	return false
}

// decodeSubTenantBody reads the 1 MiB-capped JSON body. A missing/empty body
// yields an empty struct (the field validation below rejects it with a 400).
func decodeSubTenantBody(w http.ResponseWriter, r *http.Request) (createSubTenantBody, bool) {
	var body createSubTenantBody
	if r.Body == nil {
		return body, true
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxSubTenantBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "read body: "+err.Error())
		return createSubTenantBody{}, false
	}
	if len(raw) == 0 {
		return body, true
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "malformed JSON body")
		return createSubTenantBody{}, false
	}
	return body, true
}

// validateSubTenant trims + validates the required fields and the optional
// hosting_mode, returning the client params (400 on any violation).
func validateSubTenant(w http.ResponseWriter, b createSubTenantBody) (clients.SubTenantParams, bool) {
	parent := strings.TrimSpace(b.ParentTenantID)
	name := strings.TrimSpace(b.DisplayName)
	owner := strings.TrimSpace(b.OwnerGCID)
	mode := strings.TrimSpace(b.HostingMode)

	if parent == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "parent_tenant_id required")
		return clients.SubTenantParams{}, false
	}
	if !isUUIDShape(parent) {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "parent_tenant_id must be a UUID")
		return clients.SubTenantParams{}, false
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "display_name required")
		return clients.SubTenantParams{}, false
	}
	if len(name) > 256 {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "display_name exceeds 256 characters")
		return clients.SubTenantParams{}, false
	}
	if owner == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "owner_gcid required")
		return clients.SubTenantParams{}, false
	}
	if !isUUIDShape(owner) {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "owner_gcid must be a UUID")
		return clients.SubTenantParams{}, false
	}
	if mode != "" && !validHostingModes[mode] {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "hosting_mode must be one of PLATFORM_HOSTED, WHITE_LABEL, FRANCHISE, SELF_HOST")
		return clients.SubTenantParams{}, false
	}
	codes, ok := validateAddOnCodes(w, b.AddOnCodes)
	if !ok {
		return clients.SubTenantParams{}, false
	}
	return clients.SubTenantParams{
		ParentTenantID: parent,
		DisplayName:    name,
		HostingMode:    mode,
		OwnerGCID:      owner,
		AddOnCodes:     codes,
	}, true
}

// validateAddOnCodes trims and shape-checks the optional add-on list. A blank
// entry is refused rather than dropped: it means the caller believed it was
// making a choice, and silently discarding it is how an organisation ends up
// without a feature somebody ticked. Unknown codes are NOT rejected here;
// chora-tenancy answers those with INVALID_ARGUMENT naming the code, which
// writeSubTenantErr already turns into a 400.
func validateAddOnCodes(w http.ResponseWriter, raw []string) ([]string, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	if len(raw) > maxAddOnCodeCount {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			fmt.Sprintf("add_on_codes must carry at most %d entries", maxAddOnCodeCount))
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		code := strings.TrimSpace(c)
		if code == "" {
			writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
				"add_on_codes must not carry a blank entry")
			return nil, false
		}
		if len(code) > maxAddOnCodeLen {
			writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
				fmt.Sprintf("add_on_codes entry exceeds %d characters", maxAddOnCodeLen))
			return nil, false
		}
		out = append(out, code)
	}
	return out, true
}

// writeSubTenantErr maps an upstream gRPC status to the canonical HTTP error
// envelope. FailedPrecondition (parent-invariant rejection: foreign root / cycle
// / missing parent) + AlreadyExists both map to 409 — a caller-fixable state
// conflict. (Named for this surface to keep it decoupled from writeTxErr.)
func writeSubTenantErr(w http.ResponseWriter, err error) {
	st, ok := status.FromError(unwrap(err))
	if !ok {
		writeError(w, http.StatusInternalServerError, "GATEWAY_UPSTREAM_ERROR", err.Error())
		return
	}
	switch st.Code() {
	case codes.InvalidArgument:
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", st.Message())
	case codes.FailedPrecondition:
		writeError(w, http.StatusConflict, "GATEWAY_PRECONDITION_FAILED", st.Message())
	case codes.AlreadyExists:
		writeError(w, http.StatusConflict, "GATEWAY_ALREADY_EXISTS", st.Message())
	case codes.PermissionDenied:
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN", st.Message())
	case codes.Unauthenticated:
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", st.Message())
	case codes.NotFound:
		writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND", st.Message())
	case codes.Unavailable, codes.DeadlineExceeded:
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_UPSTREAM_UNAVAILABLE", st.Message())
	default:
		writeError(w, http.StatusInternalServerError, "GATEWAY_UPSTREAM_ERROR", st.Message())
	}
}
