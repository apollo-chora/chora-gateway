// tenancy_admin_client.go — gRPC client for chora-tenancy's Tenancy service
// CreateSubTenant RPC (chora-contracts/proto/services/tenancy/v1/tenancy.proto),
// per ADR-217 Phase 2.2 (CHO-2008).
//
// chora-gateway exposes a thin operator-gated REST surface
// (tenancy_admin_handler.go, matching openapi/tenancy-admin.yaml's
// /api/v1/tenancy/sub-tenants) that proxies the franchise sub-tenant create-path
// to chora-tenancy. The HTTP handler resolves + gates the caller (platform_operator
// only) and hands the validated params here; THIS client stamps the canonical
// mesh-trust metadata on the OUTBOUND gRPC call (chora-gcid / chora-tenant-id /
// x-mesh-user-roles) via stampMesh — identical to transaction_history_client.go.
// The chora-tenancy server does NO app-layer role check (it trusts the Istio
// allow-list + the gateway gate), so the gateway is the sole authz gate.
//
// Trust model: mesh-internal (chora-gateway -> chora-tenancy across Cloud Service
// Mesh); plain HTTP/2 to the sidecar (mTLS at L4). Per feedback_no_inline_config
// the dial address comes from env (CHORA_TENANCY_GRPC_ADDR, wired in
// cmd/server/tenancy_admin_loader.go). Per feedback_no_stubs_real_wiring there is
// no in-process fake.
package clients

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"

	tenancyv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/tenancy/v1"
)

// defaultTenancyAdminCallTimeout bounds the create RPC. CreateSubTenant is a
// single atomic write (tenant + owner-member + core entitlement + outbox event)
// so p99 stays well under this.
const defaultTenancyAdminCallTimeout = 8 * time.Second

// ErrNilTenancyRPC is returned by NewTenancyAdminClient when RPC is nil.
var ErrNilTenancyRPC = errors.New("clients: tenancy gRPC client required")

// TenancyGRPCClient is the minimal slice of tenancyv1.TenancyClient the
// TenancyAdminClient depends on (satisfied by tenancyv1.NewTenancyClient(conn)).
type TenancyGRPCClient interface {
	CreateSubTenant(ctx context.Context, in *tenancyv1.CreateSubTenantRequest, opts ...grpc.CallOption) (*tenancyv1.CreateSubTenantResponse, error)
	GetTenantHierarchy(ctx context.Context, in *tenancyv1.GetTenantHierarchyRequest, opts ...grpc.CallOption) (*tenancyv1.GetTenantHierarchyResponse, error)
	ListMembershipsByGCID(ctx context.Context, in *tenancyv1.ListMembershipsByGCIDRequest, opts ...grpc.CallOption) (*tenancyv1.ListMembershipsByGCIDResponse, error)
}

// SubTenantParams is the validated create input the handler passes. HostingMode
// is the REST wire enum ("" | PLATFORM_HOSTED | WHITE_LABEL | FRANCHISE |
// SELF_HOST) — already validated against the OpenAPI enum by the handler; "" lets
// the server apply the sub-tenant default (FRANCHISE).
type SubTenantParams struct {
	ParentTenantID string
	DisplayName    string
	HostingMode    string
	OwnerGCID      string
	// AddOnCodes are the add-on CODES granted at creation beyond the mandatory
	// `core` (E1). chora-tenancy writes them as durable add_on_subscriptions
	// rows in the same transaction as the tenant. Optional; empty is the
	// historical behaviour. Membership in the catalogue is validated THERE, not
	// here: the catalogue is data seeded by a migration, so a list in the
	// gateway would be inline config that drifts.
	AddOnCodes []string
}

// TenantDTO is the JSON response (matches openapi/tenancy-admin.yaml Tenant).
type TenantDTO struct {
	TenantID       string  `json:"tenant_id"`
	ParentTenantID *string `json:"parent_tenant_id"`
	DisplayName    string  `json:"display_name"`
	HostingMode    string  `json:"hosting_mode"`
	OwnerGCID      string  `json:"owner_gcid"`
	State          string  `json:"state"`
	CreatedAt      string  `json:"created_at"`
}

// TenancyAdminClient proxies the sub-tenant create-path to chora-tenancy.
type TenancyAdminClient struct {
	rpc TenancyGRPCClient
}

// TenancyAdminClientConfig configures NewTenancyAdminClient.
type TenancyAdminClientConfig struct {
	RPC TenancyGRPCClient // REQUIRED
}

// NewTenancyAdminClient fails loud on a nil RPC.
func NewTenancyAdminClient(cfg TenancyAdminClientConfig) (*TenancyAdminClient, error) {
	if cfg.RPC == nil {
		return nil, ErrNilTenancyRPC
	}
	return &TenancyAdminClient{rpc: cfg.RPC}, nil
}

// CreateSubTenant maps params -> CreateSubTenantRequest, stamps the caller's mesh
// identity on the outbound metadata, bounds the call, and maps the response
// Tenant -> DTO. A non-empty but unrecognised HostingMode is a client-side
// validation error (the RPC is not dialled).
func (c *TenancyAdminClient) CreateSubTenant(ctx context.Context, caller Caller, p SubTenantParams) (TenantDTO, error) {
	mode, err := hostingModeToProto(p.HostingMode)
	if err != nil {
		return TenantDTO{}, err
	}

	ctx = stampMesh(ctx, caller)
	ctx, cancel := context.WithTimeout(ctx, defaultTenancyAdminCallTimeout)
	defer cancel()

	resp, err := c.rpc.CreateSubTenant(ctx, &tenancyv1.CreateSubTenantRequest{
		ParentTenantId: p.ParentTenantID,
		DisplayName:    p.DisplayName,
		HostingMode:    mode,
		OwnerGcid:      p.OwnerGCID,
		AddOnCodes:     p.AddOnCodes,
	})
	if err != nil {
		return TenantDTO{}, err
	}
	if resp == nil || resp.GetTenant() == nil {
		return TenantDTO{}, errors.New("clients: chora-tenancy returned no tenant")
	}
	return tenantToDTO(resp.GetTenant()), nil
}

// TenantHierarchyDTO is the JSON response for
// GET /api/v1/tenancy/tenants/current/hierarchy (matches chora-web
// account-lifecycle.service.ts TenantHierarchy). ADR-217 Phase 2.3b.
type TenantHierarchyDTO struct {
	IsParent bool                    `json:"is_parent"`
	Children []ChildTenantSummaryDTO `json:"children"`
}

// ChildTenantSummaryDTO is one DIRECT child of the current tenant (matches the
// FE ChildTenantSummary). atom_count is 0 for now (cross-DB to chora_creation
// is forbidden — populated later via an event-fed projection).
type ChildTenantSummaryDTO struct {
	TenantID   string `json:"tenant_id"`
	TenantName string `json:"tenant_name"`
	UserCount  int64  `json:"user_count"`
	AtomCount  int64  `json:"atom_count"`
}

// GetTenantHierarchy proxies the direct-children read to chora-tenancy, stamping
// the caller's mesh identity on the outbound gRPC call. tenantID is the caller's
// CURRENT tenant, resolved by the handler from the validated session. Children
// is always non-nil so the JSON serialises as `[]`, never `null`.
func (c *TenancyAdminClient) GetTenantHierarchy(ctx context.Context, caller Caller, tenantID string) (TenantHierarchyDTO, error) {
	ctx = stampMesh(ctx, caller)
	ctx, cancel := context.WithTimeout(ctx, defaultTenancyAdminCallTimeout)
	defer cancel()

	resp, err := c.rpc.GetTenantHierarchy(ctx, &tenancyv1.GetTenantHierarchyRequest{TenantId: tenantID})
	if err != nil {
		return TenantHierarchyDTO{}, err
	}
	out := TenantHierarchyDTO{
		IsParent: resp.GetIsParent(),
		Children: make([]ChildTenantSummaryDTO, 0, len(resp.GetChildren())),
	}
	for _, ch := range resp.GetChildren() {
		out.Children = append(out.Children, ChildTenantSummaryDTO{
			TenantID:   ch.GetTenantId(),
			TenantName: ch.GetTenantName(),
			UserCount:  ch.GetUserCount(),
			AtomCount:  ch.GetAtomCount(),
		})
	}
	return out, nil
}

// TenantMembershipDTO is one (gcid, tenant) membership as chora-tenancy sees
// it. Roles carries EVERY role held in that tenant, including `owner`, which no
// API can grant: the tenancy enum aggregation in list_memberships_by_gcid has
// always returned it.
//
// TenantSlug is what the RPC carries, and it is not the display name: the
// function falls back to the lower-cased hyphenated name only when
// tenants.slug is NULL, so it is a stable identifier for copy, not a
// human-authored title. Do not humanise it back into a name.
type TenantMembershipDTO struct {
	TenantID   string   `json:"tenant_id"`
	TenantSlug string   `json:"tenant_slug"`
	Roles      []string `json:"roles"`
}

// ListMemberships answers "which tenants does this GCID belong to, and with
// which roles" across every tenant. The subject is the gcid argument; caller is
// the mesh identity the read is attributed to, which for the operator close
// route is the OPERATOR while the subject is their target.
//
// Server-side this lands on the SECURITY DEFINER function
// list_memberships_by_gcid (chora-tenancy migration 0011), the sanctioned
// cross-tenant directory read, so no new RLS surface is opened here.
//
// includeSuspended closes the filter difference that used to be a standing
// hazard here (E3 slice 8, tenancy migration 0038). The SQL function excludes
// rows with suspended_at set, while the one-live-owner index (migration 0036)
// and the membership write guards deliberately ignore suspension, so a
// suspended owner still HOLDS the tenant while being invisible to this read.
// The closure pre-flight passes true for exactly that reason; every other
// caller passes false, because mint must keep refusing a suspended member.
//
// The widening is over suspended_at and nothing else. It is not cross-tenant
// and opens no RLS surface: the underlying SECURITY DEFINER function is the
// same one, with the same posture, from migration 0011.
func (c *TenancyAdminClient) ListMemberships(ctx context.Context, caller Caller, gcid string, includeSuspended bool) ([]TenantMembershipDTO, error) {
	subject := strings.TrimSpace(gcid)
	if subject == "" {
		return nil, errors.New("clients: ListMemberships requires a subject gcid")
	}

	ctx = stampMesh(ctx, caller)
	ctx, cancel := context.WithTimeout(ctx, defaultTenancyAdminCallTimeout)
	defer cancel()

	resp, err := c.rpc.ListMembershipsByGCID(ctx, &tenancyv1.ListMembershipsByGCIDRequest{
		Gcid:             subject,
		IncludeSuspended: includeSuspended,
	})
	if err != nil {
		return nil, err
	}
	out := make([]TenantMembershipDTO, 0, len(resp.GetMemberships()))
	for _, m := range resp.GetMemberships() {
		out = append(out, TenantMembershipDTO{
			TenantID:   m.GetTenantId(),
			TenantSlug: m.GetTenantSlug(),
			Roles:      append([]string(nil), m.GetRoles()...),
		})
	}
	return out, nil
}

// hostingModeToProto maps the REST wire enum to the proto enum. "" ->
// UNSPECIFIED (server applies the sub-tenant default). An unrecognised value is
// an error (fail-loud — the handler should have rejected it first).
func hostingModeToProto(s string) (tenancyv1.HostingMode, error) {
	switch s {
	case "":
		return tenancyv1.HostingMode_HOSTING_MODE_UNSPECIFIED, nil
	case "PLATFORM_HOSTED":
		return tenancyv1.HostingMode_HOSTING_MODE_PLATFORM_HOSTED, nil
	case "WHITE_LABEL":
		return tenancyv1.HostingMode_HOSTING_MODE_WHITE_LABEL, nil
	case "FRANCHISE":
		return tenancyv1.HostingMode_HOSTING_MODE_FRANCHISE, nil
	case "SELF_HOST":
		return tenancyv1.HostingMode_HOSTING_MODE_SELF_HOST, nil
	default:
		return tenancyv1.HostingMode_HOSTING_MODE_UNSPECIFIED, fmt.Errorf("clients: unrecognised hosting_mode %q", s)
	}
}

// hostingModeToWire is the inverse (proto enum -> REST wire string).
func hostingModeToWire(m tenancyv1.HostingMode) string {
	switch m {
	case tenancyv1.HostingMode_HOSTING_MODE_PLATFORM_HOSTED:
		return "PLATFORM_HOSTED"
	case tenancyv1.HostingMode_HOSTING_MODE_WHITE_LABEL:
		return "WHITE_LABEL"
	case tenancyv1.HostingMode_HOSTING_MODE_FRANCHISE:
		return "FRANCHISE"
	case tenancyv1.HostingMode_HOSTING_MODE_SELF_HOST:
		return "SELF_HOST"
	default:
		return ""
	}
}

// tenantStateToWire maps the proto lifecycle state to the OpenAPI wire enum.
func tenantStateToWire(s tenancyv1.TenantState) string {
	switch s {
	case tenancyv1.TenantState_TENANT_STATE_ACTIVE:
		return "ACTIVE"
	case tenancyv1.TenantState_TENANT_STATE_SUSPENDED:
		return "SUSPENDED"
	case tenancyv1.TenantState_TENANT_STATE_CLOSED:
		return "CLOSED"
	default:
		return ""
	}
}

// tenantToDTO maps a proto Tenant to the JSON DTO. parent_tenant_id is nullable
// (empty -> null) per the OpenAPI schema.
func tenantToDTO(t *tenancyv1.Tenant) TenantDTO {
	dto := TenantDTO{
		TenantID:    t.GetTenantId(),
		DisplayName: t.GetDisplayName(),
		HostingMode: hostingModeToWire(t.GetHostingMode()),
		OwnerGCID:   t.GetOwnerGcid(),
		State:       tenantStateToWire(t.GetState()),
		CreatedAt:   tsToRFC3339(t.GetCreatedAt()),
	}
	if p := t.GetParentTenantId(); p != "" {
		dto.ParentTenantID = &p
	}
	return dto
}
