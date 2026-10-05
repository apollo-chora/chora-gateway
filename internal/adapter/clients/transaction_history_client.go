// transaction_history_client.go — gRPC client for chora-tenancy's
// TransactionHistoryService (chora-contracts/proto/services/tenancy/v1/
// transaction_history.proto), per ADR-205 / CHO-1940 (Wave B4).
//
// chora-gateway exposes a thin REST surface (transaction_history_handler.go,
// matching openapi/transaction-history.yaml) that proxies the contextual
// transaction ledger to chora-tenancy. The HTTP handler resolves the caller's
// scope + mesh identity (tenant_id / gcid / roles) and hands them here; THIS
// client stamps the canonical mesh-trust metadata on the OUTBOUND gRPC call —
// the chora-tenancy server reads `chora-gcid` / `chora-tenant-id` /
// `x-mesh-user-roles` from the gRPC metadata (the proto carries NO
// caller-identity fields, by design — identity is server-injected, never
// client-supplied as a request field). This is the key difference from
// payments_client.go (which passes tenant_id/gcid as request fields).
//
// Trust model: mesh-internal (chora-gateway → chora-tenancy across Cloud
// Service Mesh); plain HTTP/2 to the sidecar (mTLS at L4). Per
// feedback_no_inline_config the dial address comes from env
// (CHORA_TENANCY_GRPC_ADDR, wired in cmd/server/transaction_history_loader.go).
// Per feedback_no_stubs_real_wiring there is no in-process fake.
package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	tenancyv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/tenancy/v1"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// defaultTxHistoryCallTimeout bounds each read RPC. The projection reads are
// indexed + keyset-paginated (ADR-205 D5) so p99 stays well under this.
const defaultTxHistoryCallTimeout = 8 * time.Second

// TxScope is the explicit ADR-205 D1 read scope the handler resolves per route.
type TxScope int

const (
	TxScopeLearner TxScope = iota + 1
	TxScopeTenant
	TxScopeMaster
)

// Caller is the authenticated mesh identity, stamped onto the outbound gRPC
// metadata so chora-tenancy binds the read scope server-side.
type Caller struct {
	GCID     string
	TenantID string
	Roles    []string
}

// TxListParams is the validated query the handler passes for List / Summary.
// Kind/Status/Sort are the REST tokens (already validated against the OpenAPI
// enums by the handler; "" = no filter). From/To are parsed timestamps (nil =
// server default window).
type TxListParams struct {
	Scope     TxScope
	Kind      string
	Status    string
	Sort      string
	From      *time.Time
	To        *time.Time
	PageSize  int32
	PageToken string
	// ManagedTenantIDs is the (repeatable) MASTER franchisee filter — OR over
	// the set. A single id is a 1-element slice (back-compat with the former
	// singular). Empty = span all franchisees.
	ManagedTenantIDs []string
	// LearnerGCIDs is the (repeatable) learner drill filter — OR over the set.
	// A single id is a 1-element slice (back-compat with the former singular).
	LearnerGCIDs []string
}

// ---- DTOs (JSON-tagged; match openapi/transaction-history.yaml) ----

type TxAmountDTO struct {
	Currency    *string `json:"currency"`
	AmountMinor int64   `json:"amount_minor"`
	ManaUnits   int64   `json:"mana_units"`
}

type TxLedgerItemDTO struct {
	LedgerID     string         `json:"ledger_id"`
	OccurredAt   string         `json:"occurred_at"`
	TenantID     string         `json:"tenant_id"`
	LearnerGCID  *string        `json:"learner_gcid"`
	Kind         string         `json:"kind"`
	SourceDomain string         `json:"source_domain"`
	SourceRefID  string         `json:"source_ref_id"`
	Label        string         `json:"label"`
	Amount       TxAmountDTO    `json:"amount"`
	Status       string         `json:"status"`
	HasDetail    bool           `json:"has_detail"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type TxListResponseDTO struct {
	Items         []TxLedgerItemDTO `json:"items"`
	NextPageToken *string           `json:"next_page_token"`
	HasMore       bool              `json:"has_more"`
}

type TxDetailItemDTO struct {
	DetailID    string         `json:"detail_id"`
	LedgerID    string         `json:"ledger_id"`
	TenantID    string         `json:"tenant_id"`
	LearnerGCID *string        `json:"learner_gcid"`
	OccurredAt  string         `json:"occurred_at"`
	ActionCode  string         `json:"action_code"`
	ManaUnits   int64          `json:"mana_units"`
	Model       *string        `json:"model"`
	TraceID     *string        `json:"trace_id"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type TxDetailResponseDTO struct {
	Parent        TxLedgerItemDTO   `json:"parent"`
	Details       []TxDetailItemDTO `json:"details"`
	NextPageToken *string           `json:"next_page_token"`
	HasMore       bool              `json:"has_more"`
}

type TxSummaryDTO struct {
	TotalCount            int64            `json:"total_count"`
	AmountMinorByCurrency map[string]int64 `json:"amount_minor_by_currency"`
	TotalManaToppedUp     int64            `json:"total_mana_topped_up"`
	TotalManaSpent        int64            `json:"total_mana_spent"`
	CountByKind           map[string]int64 `json:"count_by_kind"`
	CountByStatus         map[string]int64 `json:"count_by_status"`
	WindowFrom            string           `json:"window_from"`
	WindowTo              string           `json:"window_to"`
}

// TxExportJobDTO is the async export handle (the OpenAPI ExportJob shape).
type TxExportJobDTO struct {
	JobID       string  `json:"job_id"`
	Status      string  `json:"status"` // pending | building | ready | failed
	Format      string  `json:"format"` // csv | json
	CreatedAt   string  `json:"created_at"`
	DownloadURL *string `json:"download_url"`
	ExpiresAt   *string `json:"expires_at"`
}

// TransactionHistoryGRPCClient is the minimal slice of
// tenancyv1.TransactionHistoryServiceClient chora-gateway calls (Stream is the
// BFF's own SSE concern, not proxied 1:1). Tests inject a fake.
type TransactionHistoryGRPCClient interface {
	ListTransactions(ctx context.Context, in *tenancyv1.ListTransactionsRequest, opts ...grpc.CallOption) (*tenancyv1.ListTransactionsResponse, error)
	GetTransactionDetail(ctx context.Context, in *tenancyv1.GetTransactionDetailRequest, opts ...grpc.CallOption) (*tenancyv1.GetTransactionDetailResponse, error)
	GetSummary(ctx context.Context, in *tenancyv1.GetSummaryRequest, opts ...grpc.CallOption) (*tenancyv1.GetSummaryResponse, error)
	CreateTransactionExport(ctx context.Context, in *tenancyv1.CreateTransactionExportRequest, opts ...grpc.CallOption) (*tenancyv1.TransactionExportJob, error)
	GetTransactionExport(ctx context.Context, in *tenancyv1.GetTransactionExportRequest, opts ...grpc.CallOption) (*tenancyv1.TransactionExportJob, error)
	ListFranchisees(ctx context.Context, in *tenancyv1.ListFranchiseesRequest, opts ...grpc.CallOption) (*tenancyv1.ListFranchiseesResponse, error)
}

// TransactionHistoryClient adapts the gRPC service to the HTTP-handler-facing
// API + stamps mesh identity on every call.
type TransactionHistoryClient struct {
	rpc     TransactionHistoryGRPCClient
	timeout time.Duration
}

// TransactionHistoryClientConfig is the constructor input.
type TransactionHistoryClientConfig struct {
	RPC     TransactionHistoryGRPCClient // REQUIRED
	Timeout time.Duration                // default 8s
}

// NewTransactionHistoryClient fails loud on a nil RPC (boot must not proceed
// with a half-wired client).
func NewTransactionHistoryClient(cfg TransactionHistoryClientConfig) (*TransactionHistoryClient, error) {
	if cfg.RPC == nil {
		return nil, errors.New("clients.transaction_history: RPC client required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTxHistoryCallTimeout
	}
	return &TransactionHistoryClient{rpc: cfg.RPC, timeout: timeout}, nil
}

// stampMesh appends the canonical mesh-trust identity headers to the outbound
// gRPC metadata. chora-tenancy trusts these from the BFF mTLS peer + binds the
// read scope from them (NEVER from a request field).
func stampMesh(ctx context.Context, caller Caller) context.Context {
	kv := []string{
		servicemesh.HeaderGCID, caller.GCID,
		servicemesh.HeaderTenantID, caller.TenantID,
	}
	if roles := cleanRoles(caller.Roles); roles != "" {
		kv = append(kv, servicemesh.HeaderUserRoles, roles)
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}

func cleanRoles(roles []string) string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return strings.Join(out, ",")
}

// TxFranchiseeDTO is one franchisee row for the master's filter picker.
type TxFranchiseeDTO struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

// TxFranchiseesDTO is the ListFranchisees response the BFF returns to the FE.
type TxFranchiseesDTO struct {
	Franchisees   []TxFranchiseeDTO `json:"franchisees"`
	NextPageToken string            `json:"next_page_token"`
}

// ListFranchisees proxies ListFranchisees — the master's franchisee directory
// backing the O+/H+ franchisee multi-select filter.
func (c *TransactionHistoryClient) ListFranchisees(ctx context.Context, caller Caller, q string, pageSize int32, pageToken string) (TxFranchiseesDTO, error) {
	if c == nil || c.rpc == nil {
		return TxFranchiseesDTO{}, errors.New("transaction history client: rpc nil")
	}
	callCtx, cancel := context.WithTimeout(stampMesh(ctx, caller), c.timeout)
	defer cancel()
	resp, err := c.rpc.ListFranchisees(callCtx, &tenancyv1.ListFranchiseesRequest{
		Q:         q,
		PageSize:  pageSize,
		PageToken: pageToken,
	})
	if err != nil {
		return TxFranchiseesDTO{}, fmt.Errorf("transaction history client: ListFranchisees: %w", err)
	}
	if resp == nil {
		return TxFranchiseesDTO{}, errors.New("transaction history client: nil franchisees response")
	}
	items := make([]TxFranchiseeDTO, 0, len(resp.GetFranchisees()))
	for _, f := range resp.GetFranchisees() {
		items = append(items, TxFranchiseeDTO{TenantID: f.GetTenantId(), Name: f.GetName()})
	}
	return TxFranchiseesDTO{Franchisees: items, NextPageToken: resp.GetNextPageToken()}, nil
}

// List proxies ListTransactions.
func (c *TransactionHistoryClient) List(ctx context.Context, caller Caller, p TxListParams) (TxListResponseDTO, error) {
	if c == nil || c.rpc == nil {
		return TxListResponseDTO{}, errors.New("transaction history client: rpc nil")
	}
	callCtx, cancel := context.WithTimeout(stampMesh(ctx, caller), c.timeout)
	defer cancel()
	resp, err := c.rpc.ListTransactions(callCtx, &tenancyv1.ListTransactionsRequest{
		Scope:     scopeToProto(p.Scope),
		Filters:   filtersToProto(p),
		PageToken: p.PageToken,
		PageSize:  p.PageSize,
	})
	if err != nil {
		return TxListResponseDTO{}, fmt.Errorf("transaction history client: ListTransactions: %w", err)
	}
	if resp == nil {
		return TxListResponseDTO{}, errors.New("transaction history client: nil list response")
	}
	items := make([]TxLedgerItemDTO, 0, len(resp.GetItems()))
	for _, it := range resp.GetItems() {
		items = append(items, ledgerItemToDTO(it))
	}
	return TxListResponseDTO{
		Items:         items,
		NextPageToken: nilIfEmpty(resp.GetNextPageToken()),
		HasMore:       resp.GetHasMore(),
	}, nil
}

// Detail proxies GetTransactionDetail. A codes.NotFound surfaces as a wrapped
// gRPC status the handler maps to 404.
func (c *TransactionHistoryClient) Detail(ctx context.Context, caller Caller, scope TxScope, ledgerID, pageToken string, pageSize int32) (TxDetailResponseDTO, error) {
	if c == nil || c.rpc == nil {
		return TxDetailResponseDTO{}, errors.New("transaction history client: rpc nil")
	}
	callCtx, cancel := context.WithTimeout(stampMesh(ctx, caller), c.timeout)
	defer cancel()
	resp, err := c.rpc.GetTransactionDetail(callCtx, &tenancyv1.GetTransactionDetailRequest{
		Scope:     scopeToProto(scope),
		LedgerId:  ledgerID,
		PageToken: pageToken,
		PageSize:  pageSize,
	})
	if err != nil {
		return TxDetailResponseDTO{}, fmt.Errorf("transaction history client: GetTransactionDetail: %w", err)
	}
	if resp == nil {
		return TxDetailResponseDTO{}, errors.New("transaction history client: nil detail response")
	}
	details := make([]TxDetailItemDTO, 0, len(resp.GetDetails()))
	for _, d := range resp.GetDetails() {
		details = append(details, detailItemToDTO(d))
	}
	out := TxDetailResponseDTO{
		Details:       details,
		NextPageToken: nilIfEmpty(resp.GetNextPageToken()),
		HasMore:       resp.GetHasMore(),
	}
	if resp.GetParent() != nil {
		out.Parent = ledgerItemToDTO(resp.GetParent())
	}
	return out, nil
}

// Summary proxies GetSummary.
func (c *TransactionHistoryClient) Summary(ctx context.Context, caller Caller, p TxListParams) (TxSummaryDTO, error) {
	if c == nil || c.rpc == nil {
		return TxSummaryDTO{}, errors.New("transaction history client: rpc nil")
	}
	callCtx, cancel := context.WithTimeout(stampMesh(ctx, caller), c.timeout)
	defer cancel()
	resp, err := c.rpc.GetSummary(callCtx, &tenancyv1.GetSummaryRequest{
		Scope:   scopeToProto(p.Scope),
		Filters: filtersToProto(p),
	})
	if err != nil {
		return TxSummaryDTO{}, fmt.Errorf("transaction history client: GetSummary: %w", err)
	}
	if resp == nil || resp.GetSummary() == nil {
		return TxSummaryDTO{}, errors.New("transaction history client: nil summary response")
	}
	return summaryToDTO(resp.GetSummary()), nil
}

// CreateExport enqueues an async export (202 → poll). format is the REST token
// (csv|json); p carries the same filters as List.
func (c *TransactionHistoryClient) CreateExport(ctx context.Context, caller Caller, p TxListParams, format string) (TxExportJobDTO, error) {
	if c == nil || c.rpc == nil {
		return TxExportJobDTO{}, errors.New("transaction history client: rpc nil")
	}
	callCtx, cancel := context.WithTimeout(stampMesh(ctx, caller), c.timeout)
	defer cancel()
	resp, err := c.rpc.CreateTransactionExport(callCtx, &tenancyv1.CreateTransactionExportRequest{
		Scope:   scopeToProto(p.Scope),
		Filters: filtersToProto(p),
		Format:  tokenToFormat(format),
	})
	if err != nil {
		return TxExportJobDTO{}, fmt.Errorf("transaction history client: CreateTransactionExport: %w", err)
	}
	if resp == nil {
		return TxExportJobDTO{}, errors.New("transaction history client: nil export response")
	}
	return exportJobToDTO(resp), nil
}

// GetExport polls one export job.
func (c *TransactionHistoryClient) GetExport(ctx context.Context, caller Caller, scope TxScope, jobID string) (TxExportJobDTO, error) {
	if c == nil || c.rpc == nil {
		return TxExportJobDTO{}, errors.New("transaction history client: rpc nil")
	}
	callCtx, cancel := context.WithTimeout(stampMesh(ctx, caller), c.timeout)
	defer cancel()
	resp, err := c.rpc.GetTransactionExport(callCtx, &tenancyv1.GetTransactionExportRequest{
		Scope: scopeToProto(scope),
		JobId: jobID,
	})
	if err != nil {
		return TxExportJobDTO{}, fmt.Errorf("transaction history client: GetTransactionExport: %w", err)
	}
	if resp == nil {
		return TxExportJobDTO{}, errors.New("transaction history client: nil export response")
	}
	return exportJobToDTO(resp), nil
}

// ---- proto mapping ----

func tokenToFormat(t string) tenancyv1.ExportFormat {
	switch t {
	case "csv":
		return tenancyv1.ExportFormat_EXPORT_FORMAT_CSV
	case "json":
		return tenancyv1.ExportFormat_EXPORT_FORMAT_JSON
	default:
		return tenancyv1.ExportFormat_EXPORT_FORMAT_UNSPECIFIED
	}
}

func formatToToken(f tenancyv1.ExportFormat) string {
	switch f {
	case tenancyv1.ExportFormat_EXPORT_FORMAT_CSV:
		return "csv"
	case tenancyv1.ExportFormat_EXPORT_FORMAT_JSON:
		return "json"
	default:
		return ""
	}
}

func exportStatusToToken(s tenancyv1.ExportStatus) string {
	switch s {
	case tenancyv1.ExportStatus_EXPORT_STATUS_PENDING:
		return "pending"
	case tenancyv1.ExportStatus_EXPORT_STATUS_BUILDING:
		return "building"
	case tenancyv1.ExportStatus_EXPORT_STATUS_READY:
		return "ready"
	case tenancyv1.ExportStatus_EXPORT_STATUS_FAILED:
		return "failed"
	default:
		return ""
	}
}

func exportJobToDTO(j *tenancyv1.TransactionExportJob) TxExportJobDTO {
	dto := TxExportJobDTO{
		JobID:       j.GetJobId(),
		Status:      exportStatusToToken(j.GetStatus()),
		Format:      formatToToken(j.GetFormat()),
		CreatedAt:   tsToRFC3339(j.GetCreatedAt()),
		DownloadURL: nilIfEmpty(j.GetDownloadUrl()),
	}
	if exp := tsToRFC3339(j.GetExpiresAt()); exp != "" {
		dto.ExpiresAt = &exp
	}
	return dto
}

func scopeToProto(s TxScope) tenancyv1.TransactionScope {
	switch s {
	case TxScopeLearner:
		return tenancyv1.TransactionScope_TRANSACTION_SCOPE_LEARNER
	case TxScopeTenant:
		return tenancyv1.TransactionScope_TRANSACTION_SCOPE_TENANT
	case TxScopeMaster:
		return tenancyv1.TransactionScope_TRANSACTION_SCOPE_MASTER
	default:
		return tenancyv1.TransactionScope_TRANSACTION_SCOPE_UNSPECIFIED
	}
}

func filtersToProto(p TxListParams) *tenancyv1.TransactionFilters {
	f := &tenancyv1.TransactionFilters{
		Kind:             tokenToKind(p.Kind),
		Status:           tokenToStatus(p.Status),
		Sort:             p.Sort,
		ManagedTenantIds: p.ManagedTenantIDs,
		LearnerGcids:     p.LearnerGCIDs,
	}
	if p.From != nil {
		f.From = timestamppb.New(p.From.UTC())
	}
	if p.To != nil {
		f.To = timestamppb.New(p.To.UTC())
	}
	return f
}

func tokenToKind(t string) tenancyv1.TransactionKind {
	switch t {
	case "purchase":
		return tenancyv1.TransactionKind_TRANSACTION_KIND_PURCHASE
	case "mana_topup":
		return tenancyv1.TransactionKind_TRANSACTION_KIND_MANA_TOPUP
	case "mana_spend_daily":
		return tenancyv1.TransactionKind_TRANSACTION_KIND_MANA_SPEND_DAILY
	default:
		return tenancyv1.TransactionKind_TRANSACTION_KIND_UNSPECIFIED
	}
}

func tokenToStatus(t string) tenancyv1.TransactionStatus {
	switch t {
	case "captured":
		return tenancyv1.TransactionStatus_TRANSACTION_STATUS_CAPTURED
	case "refunded":
		return tenancyv1.TransactionStatus_TRANSACTION_STATUS_REFUNDED
	case "failed":
		return tenancyv1.TransactionStatus_TRANSACTION_STATUS_FAILED
	case "expired":
		return tenancyv1.TransactionStatus_TRANSACTION_STATUS_EXPIRED
	case "posted":
		return tenancyv1.TransactionStatus_TRANSACTION_STATUS_POSTED
	default: // "" | "all"
		return tenancyv1.TransactionStatus_TRANSACTION_STATUS_UNSPECIFIED
	}
}

func kindToToken(k tenancyv1.TransactionKind) string {
	switch k {
	case tenancyv1.TransactionKind_TRANSACTION_KIND_PURCHASE:
		return "purchase"
	case tenancyv1.TransactionKind_TRANSACTION_KIND_MANA_TOPUP:
		return "mana_topup"
	case tenancyv1.TransactionKind_TRANSACTION_KIND_MANA_SPEND_DAILY:
		return "mana_spend_daily"
	default:
		return ""
	}
}

func statusToToken(s tenancyv1.TransactionStatus) string {
	switch s {
	case tenancyv1.TransactionStatus_TRANSACTION_STATUS_CAPTURED:
		return "captured"
	case tenancyv1.TransactionStatus_TRANSACTION_STATUS_REFUNDED:
		return "refunded"
	case tenancyv1.TransactionStatus_TRANSACTION_STATUS_FAILED:
		return "failed"
	case tenancyv1.TransactionStatus_TRANSACTION_STATUS_EXPIRED:
		return "expired"
	case tenancyv1.TransactionStatus_TRANSACTION_STATUS_POSTED:
		return "posted"
	default:
		return ""
	}
}

func ledgerItemToDTO(it *tenancyv1.TransactionLedgerItem) TxLedgerItemDTO {
	dto := TxLedgerItemDTO{
		LedgerID:     it.GetLedgerId(),
		OccurredAt:   tsToRFC3339(it.GetOccurredAt()),
		TenantID:     it.GetTenantId(),
		LearnerGCID:  nilIfEmpty(it.GetLearnerGcid()),
		Kind:         kindToToken(it.GetKind()),
		SourceDomain: it.GetSourceDomain(),
		SourceRefID:  it.GetSourceRefId(),
		Label:        it.GetLabel(),
		Status:       statusToToken(it.GetStatus()),
		HasDetail:    it.GetHasDetail(),
		Metadata:     parseMetadata(it.GetMetadataJson()),
	}
	if a := it.GetAmount(); a != nil {
		dto.Amount = TxAmountDTO{
			Currency:    nilIfEmpty(a.GetCurrency()),
			AmountMinor: a.GetAmountMinor(),
			ManaUnits:   a.GetManaUnits(),
		}
	}
	return dto
}

func detailItemToDTO(d *tenancyv1.TransactionDetailItem) TxDetailItemDTO {
	return TxDetailItemDTO{
		DetailID:    d.GetDetailId(),
		LedgerID:    d.GetLedgerId(),
		TenantID:    d.GetTenantId(),
		LearnerGCID: nilIfEmpty(d.GetLearnerGcid()),
		OccurredAt:  tsToRFC3339(d.GetOccurredAt()),
		ActionCode:  d.GetActionCode(),
		ManaUnits:   d.GetManaUnits(),
		Model:       nilIfEmpty(d.GetModel()),
		TraceID:     nilIfEmpty(d.GetTraceId()),
		Metadata:    parseMetadata(d.GetMetadataJson()),
	}
}

// summaryToDTO re-keys the gRPC count maps (keyed by the proto enum NAME, e.g.
// "TRANSACTION_KIND_PURCHASE") onto the REST contract keys (the lowercase enum
// VALUE, e.g. "purchase"). UNSPECIFIED / unknown keys are dropped.
func summaryToDTO(s *tenancyv1.TransactionSummary) TxSummaryDTO {
	out := TxSummaryDTO{
		TotalCount:            s.GetTotalCount(),
		AmountMinorByCurrency: nonNilInt64Map(s.GetAmountMinorByCurrency()),
		TotalManaToppedUp:     s.GetTotalManaToppedUp(),
		TotalManaSpent:        s.GetTotalManaSpent(),
		CountByKind:           map[string]int64{},
		CountByStatus:         map[string]int64{},
		WindowFrom:            tsToRFC3339(s.GetWindowFrom()),
		WindowTo:              tsToRFC3339(s.GetWindowTo()),
	}
	for name, v := range s.GetCountByKind() {
		if tok := kindToToken(tenancyv1.TransactionKind(tenancyv1.TransactionKind_value[name])); tok != "" {
			out.CountByKind[tok] = v
		}
	}
	for name, v := range s.GetCountByStatus() {
		if tok := statusToToken(tenancyv1.TransactionStatus(tenancyv1.TransactionStatus_value[name])); tok != "" {
			out.CountByStatus[tok] = v
		}
	}
	return out
}

func nonNilInt64Map(m map[string]int64) map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return m
}

func tsToRFC3339(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339Nano)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseMetadata unmarshals the projection's metadata_json string into a map for
// the REST `metadata` object. Empty / "{}" → nil (omitted via omitempty). A
// malformed blob (should not happen — the projection writes valid JSONB) → nil
// rather than 500-ing the whole page on one auxiliary display field.
func parseMetadata(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
