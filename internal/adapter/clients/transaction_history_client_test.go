package clients_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	tenancyv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/tenancy/v1"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

type fakeTxRPC struct {
	gotCtx          context.Context
	gotListReq      *tenancyv1.ListTransactionsRequest
	gotDetReq       *tenancyv1.GetTransactionDetailRequest
	listResp        *tenancyv1.ListTransactionsResponse
	detailResp      *tenancyv1.GetTransactionDetailResponse
	summaryResp     *tenancyv1.GetSummaryResponse
	gotCreateExport *tenancyv1.CreateTransactionExportRequest
	gotGetExport    *tenancyv1.GetTransactionExportRequest
	exportResp      *tenancyv1.TransactionExportJob
	gotFranchisees  *tenancyv1.ListFranchiseesRequest
	franchiseesResp *tenancyv1.ListFranchiseesResponse
	err             error
}

func (f *fakeTxRPC) ListFranchisees(ctx context.Context, in *tenancyv1.ListFranchiseesRequest, _ ...grpc.CallOption) (*tenancyv1.ListFranchiseesResponse, error) {
	f.gotCtx, f.gotFranchisees = ctx, in
	return f.franchiseesResp, f.err
}

func (f *fakeTxRPC) ListTransactions(ctx context.Context, in *tenancyv1.ListTransactionsRequest, _ ...grpc.CallOption) (*tenancyv1.ListTransactionsResponse, error) {
	f.gotCtx, f.gotListReq = ctx, in
	return f.listResp, f.err
}
func (f *fakeTxRPC) GetTransactionDetail(ctx context.Context, in *tenancyv1.GetTransactionDetailRequest, _ ...grpc.CallOption) (*tenancyv1.GetTransactionDetailResponse, error) {
	f.gotCtx, f.gotDetReq = ctx, in
	return f.detailResp, f.err
}
func (f *fakeTxRPC) GetSummary(ctx context.Context, in *tenancyv1.GetSummaryRequest, _ ...grpc.CallOption) (*tenancyv1.GetSummaryResponse, error) {
	f.gotCtx = ctx
	return f.summaryResp, f.err
}
func (f *fakeTxRPC) CreateTransactionExport(ctx context.Context, in *tenancyv1.CreateTransactionExportRequest, _ ...grpc.CallOption) (*tenancyv1.TransactionExportJob, error) {
	f.gotCtx, f.gotCreateExport = ctx, in
	return f.exportResp, f.err
}
func (f *fakeTxRPC) GetTransactionExport(ctx context.Context, in *tenancyv1.GetTransactionExportRequest, _ ...grpc.CallOption) (*tenancyv1.TransactionExportJob, error) {
	f.gotCtx, f.gotGetExport = ctx, in
	return f.exportResp, f.err
}

func newClient(t *testing.T, rpc clients.TransactionHistoryGRPCClient) *clients.TransactionHistoryClient {
	t.Helper()
	c, err := clients.NewTransactionHistoryClient(clients.TransactionHistoryClientConfig{RPC: rpc})
	if err != nil {
		t.Fatalf("NewTransactionHistoryClient: %v", err)
	}
	return c
}

func TestList_StampsMeshMetadata_AndMapsProto(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	cur := "sgd"
	fake := &fakeTxRPC{listResp: &tenancyv1.ListTransactionsResponse{
		Items: []*tenancyv1.TransactionLedgerItem{{
			LedgerId:     "L1",
			OccurredAt:   timestamppb.New(from),
			TenantId:     "t1",
			Kind:         tenancyv1.TransactionKind_TRANSACTION_KIND_PURCHASE,
			SourceDomain: "payments",
			SourceRefId:  "pur1",
			Label:        "Course X",
			Amount:       &tenancyv1.TransactionAmount{Currency: cur, AmountMinor: 4999},
			Status:       tenancyv1.TransactionStatus_TRANSACTION_STATUS_CAPTURED,
			MetadataJson: `{"course_id":"c1"}`,
		}},
		NextPageToken: "tok2",
		HasMore:       true,
	}}
	c := newClient(t, fake)

	caller := clients.Caller{GCID: "g1", TenantID: "t1", Roles: []string{"platform_operator", "learner", " "}}
	resp, err := c.List(context.Background(), caller, clients.TxListParams{
		Scope: clients.TxScopeMaster, Kind: "purchase", From: &from, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// mesh metadata stamped on the outbound ctx
	md, ok := metadata.FromOutgoingContext(fake.gotCtx)
	if !ok {
		t.Fatal("no outgoing metadata stamped")
	}
	if got := md.Get("chora-gcid"); len(got) != 1 || got[0] != "g1" {
		t.Errorf("chora-gcid = %v", got)
	}
	if got := md.Get("chora-tenant-id"); len(got) != 1 || got[0] != "t1" {
		t.Errorf("chora-tenant-id = %v", got)
	}
	if got := md.Get("x-mesh-user-roles"); len(got) != 1 || got[0] != "platform_operator,learner" {
		t.Errorf("x-mesh-user-roles = %v (blank role should be dropped)", got)
	}

	// params → proto
	if fake.gotListReq.GetScope() != tenancyv1.TransactionScope_TRANSACTION_SCOPE_MASTER {
		t.Errorf("scope = %v", fake.gotListReq.GetScope())
	}
	if fake.gotListReq.GetFilters().GetKind() != tenancyv1.TransactionKind_TRANSACTION_KIND_PURCHASE {
		t.Errorf("kind filter not mapped")
	}
	if !fake.gotListReq.GetFilters().GetFrom().AsTime().Equal(from) {
		t.Errorf("from filter not mapped")
	}

	// proto → DTO
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d", len(resp.Items))
	}
	it := resp.Items[0]
	if it.Kind != "purchase" || it.Status != "captured" {
		t.Errorf("enum tokens wrong: %s/%s", it.Kind, it.Status)
	}
	if it.Amount.Currency == nil || *it.Amount.Currency != "sgd" || it.Amount.AmountMinor != 4999 {
		t.Errorf("amount wrong: %+v", it.Amount)
	}
	if it.Metadata["course_id"] != "c1" {
		t.Errorf("metadata not parsed to object: %v", it.Metadata)
	}
	if it.LearnerGCID != nil {
		t.Errorf("empty learner_gcid should be nil, got %v", *it.LearnerGCID)
	}
	if resp.NextPageToken == nil || *resp.NextPageToken != "tok2" || !resp.HasMore {
		t.Errorf("pagination wrong: %v / %v", resp.NextPageToken, resp.HasMore)
	}
}

func TestList_MultiLearner_MapsSliceToProto(t *testing.T) {
	fake := &fakeTxRPC{listResp: &tenancyv1.ListTransactionsResponse{}}
	c := newClient(t, fake)
	learners := []string{"g-a", "g-b"}
	_, err := c.List(context.Background(), clients.Caller{GCID: "g", TenantID: "t"}, clients.TxListParams{
		Scope: clients.TxScopeTenant, LearnerGCIDs: learners,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := fake.gotListReq.GetFilters().GetLearnerGcids()
	if len(got) != 2 || got[0] != "g-a" || got[1] != "g-b" {
		t.Errorf("learner_gcids not mapped to proto: %v", got)
	}
}

func TestList_MultiFranchisee_MapsSliceToProto(t *testing.T) {
	fake := &fakeTxRPC{listResp: &tenancyv1.ListTransactionsResponse{}}
	c := newClient(t, fake)
	franchisees := []string{"f-a", "f-b"}
	_, err := c.List(context.Background(), clients.Caller{GCID: "g", TenantID: "t"}, clients.TxListParams{
		Scope: clients.TxScopeMaster, ManagedTenantIDs: franchisees,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := fake.gotListReq.GetFilters().GetManagedTenantIds()
	if len(got) != 2 || got[0] != "f-a" || got[1] != "f-b" {
		t.Errorf("managed_tenant_ids not mapped to proto: %v", got)
	}
}

func TestList_EmptyNextPageToken_IsNil(t *testing.T) {
	fake := &fakeTxRPC{listResp: &tenancyv1.ListTransactionsResponse{NextPageToken: "", HasMore: false}}
	c := newClient(t, fake)
	resp, err := c.List(context.Background(), clients.Caller{GCID: "g", TenantID: "t"}, clients.TxListParams{Scope: clients.TxScopeLearner})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.NextPageToken != nil {
		t.Errorf("empty next_page_token must map to nil, got %v", *resp.NextPageToken)
	}
}

func TestSummary_ReKeysCountMapsToRestTokens(t *testing.T) {
	fake := &fakeTxRPC{summaryResp: &tenancyv1.GetSummaryResponse{Summary: &tenancyv1.TransactionSummary{
		TotalCount:            3,
		AmountMinorByCurrency: map[string]int64{"sgd": 8998},
		TotalManaToppedUp:     500,
		TotalManaSpent:        30,
		CountByKind: map[string]int64{
			"TRANSACTION_KIND_PURCHASE":         2,
			"TRANSACTION_KIND_MANA_SPEND_DAILY": 1,
			"TRANSACTION_KIND_UNSPECIFIED":      9, // must be dropped
		},
		CountByStatus: map[string]int64{
			"TRANSACTION_STATUS_CAPTURED": 2,
			"TRANSACTION_STATUS_POSTED":   1,
		},
		WindowFrom: timestamppb.New(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)),
		WindowTo:   timestamppb.New(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)),
	}}}
	c := newClient(t, fake)

	sum, err := c.Summary(context.Background(), clients.Caller{GCID: "g", TenantID: "t"}, clients.TxListParams{Scope: clients.TxScopeTenant})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.CountByKind["purchase"] != 2 || sum.CountByKind["mana_spend_daily"] != 1 {
		t.Errorf("count_by_kind re-key wrong: %v", sum.CountByKind)
	}
	if _, leaked := sum.CountByKind["TRANSACTION_KIND_UNSPECIFIED"]; leaked {
		t.Errorf("enum-name key leaked into REST map: %v", sum.CountByKind)
	}
	if _, leaked := sum.CountByKind[""]; leaked {
		t.Errorf("UNSPECIFIED kind should be dropped, not mapped to empty key")
	}
	if sum.CountByStatus["captured"] != 2 || sum.CountByStatus["posted"] != 1 {
		t.Errorf("count_by_status re-key wrong: %v", sum.CountByStatus)
	}
	if sum.AmountMinorByCurrency["sgd"] != 8998 || sum.TotalManaSpent != 30 {
		t.Errorf("summary scalars wrong: %+v", sum)
	}
	if sum.WindowFrom == "" || sum.WindowTo == "" {
		t.Errorf("window timestamps not formatted: %q/%q", sum.WindowFrom, sum.WindowTo)
	}
}

func TestDetail_MapsParentAndDetails(t *testing.T) {
	fake := &fakeTxRPC{detailResp: &tenancyv1.GetTransactionDetailResponse{
		Parent: &tenancyv1.TransactionLedgerItem{
			LedgerId: "L1", Kind: tenancyv1.TransactionKind_TRANSACTION_KIND_MANA_SPEND_DAILY,
			Status: tenancyv1.TransactionStatus_TRANSACTION_STATUS_POSTED, HasDetail: true,
			OccurredAt: timestamppb.New(time.Now().UTC()),
		},
		Details: []*tenancyv1.TransactionDetailItem{{
			DetailId: "D1", LedgerId: "L1", ActionCode: "question_generation", ManaUnits: 30,
			OccurredAt: timestamppb.New(time.Now().UTC()),
		}},
	}}
	c := newClient(t, fake)
	resp, err := c.Detail(context.Background(), clients.Caller{GCID: "g", TenantID: "t"}, clients.TxScopeLearner, "L1", "", 50)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if fake.gotDetReq.GetLedgerId() != "L1" || fake.gotDetReq.GetScope() != tenancyv1.TransactionScope_TRANSACTION_SCOPE_LEARNER {
		t.Errorf("detail req wrong: %+v", fake.gotDetReq)
	}
	if resp.Parent.LedgerID != "L1" || !resp.Parent.HasDetail || resp.Parent.Kind != "mana_spend_daily" {
		t.Errorf("parent wrong: %+v", resp.Parent)
	}
	if len(resp.Details) != 1 || resp.Details[0].ActionCode != "question_generation" {
		t.Errorf("details wrong: %+v", resp.Details)
	}
}

func TestCreateExport_MapsFormatAndStampsMesh(t *testing.T) {
	fake := &fakeTxRPC{exportResp: &tenancyv1.TransactionExportJob{
		JobId: "EJ1", Status: tenancyv1.ExportStatus_EXPORT_STATUS_PENDING,
		Format: tenancyv1.ExportFormat_EXPORT_FORMAT_CSV, CreatedAt: timestamppb.New(time.Now()),
	}}
	c := newClient(t, fake)
	dto, err := c.CreateExport(context.Background(), clients.Caller{GCID: "g", TenantID: "t", Roles: []string{"platform_operator"}},
		clients.TxListParams{Scope: clients.TxScopeMaster, Kind: "purchase"}, "csv")
	if err != nil {
		t.Fatalf("CreateExport: %v", err)
	}
	if fake.gotCreateExport.GetFormat() != tenancyv1.ExportFormat_EXPORT_FORMAT_CSV {
		t.Errorf("format not mapped: %v", fake.gotCreateExport.GetFormat())
	}
	if fake.gotCreateExport.GetScope() != tenancyv1.TransactionScope_TRANSACTION_SCOPE_MASTER {
		t.Errorf("scope not mapped")
	}
	if md, ok := metadata.FromOutgoingContext(fake.gotCtx); !ok || md.Get("chora-gcid")[0] != "g" {
		t.Errorf("mesh metadata not stamped on export call")
	}
	if dto.JobID != "EJ1" || dto.Status != "pending" || dto.Format != "csv" {
		t.Errorf("export dto wrong: %+v", dto)
	}
}

func TestGetExport_MapsReadyURL(t *testing.T) {
	fake := &fakeTxRPC{exportResp: &tenancyv1.TransactionExportJob{
		JobId: "EJ2", Status: tenancyv1.ExportStatus_EXPORT_STATUS_READY,
		Format: tenancyv1.ExportFormat_EXPORT_FORMAT_JSON, DownloadUrl: "https://signed/x",
		ExpiresAt: timestamppb.New(time.Now()), CreatedAt: timestamppb.New(time.Now()),
	}}
	c := newClient(t, fake)
	dto, err := c.GetExport(context.Background(), clients.Caller{GCID: "g", TenantID: "t"}, clients.TxScopeTenant, "EJ2")
	if err != nil {
		t.Fatalf("GetExport: %v", err)
	}
	if fake.gotGetExport.GetJobId() != "EJ2" {
		t.Errorf("job id not passed")
	}
	if dto.Status != "ready" || dto.DownloadURL == nil || *dto.DownloadURL != "https://signed/x" || dto.ExpiresAt == nil {
		t.Errorf("ready export dto wrong: %+v", dto)
	}
}

func TestNewClient_NilRPC_FailsLoud(t *testing.T) {
	if _, err := clients.NewTransactionHistoryClient(clients.TransactionHistoryClientConfig{}); err == nil {
		t.Fatal("expected error on nil RPC")
	}
}

func TestListFranchisees_MapsResponseAndForwardsQuery(t *testing.T) {
	fake := &fakeTxRPC{franchiseesResp: &tenancyv1.ListFranchiseesResponse{
		Franchisees: []*tenancyv1.Franchisee{
			{TenantId: "t-a", Name: "Acme"},
			{TenantId: "t-b", Name: "Beta"},
		},
		NextPageToken: "50",
	}}
	c := newClient(t, fake)
	dto, err := c.ListFranchisees(
		context.Background(),
		clients.Caller{GCID: "g-op", Roles: []string{"platform_operator"}},
		"ac", 50, "",
	)
	if err != nil {
		t.Fatalf("ListFranchisees: %v", err)
	}
	if len(dto.Franchisees) != 2 || dto.Franchisees[0].TenantID != "t-a" || dto.Franchisees[0].Name != "Acme" {
		t.Errorf("mapped franchisees = %+v", dto.Franchisees)
	}
	if dto.NextPageToken != "50" {
		t.Errorf("next_page_token = %q; want 50", dto.NextPageToken)
	}
	if fake.gotFranchisees.GetQ() != "ac" || fake.gotFranchisees.GetPageSize() != 50 {
		t.Errorf("request not forwarded: %+v", fake.gotFranchisees)
	}
}
