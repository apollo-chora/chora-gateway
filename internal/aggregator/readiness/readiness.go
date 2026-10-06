// Package readiness aggregates the instance-readiness view for the H+
// operator and tenant-admin screens (UX refactor Phase E, package E2).
//
// Separate from the medashboard aggregator on purpose. That one serves the
// LEARNER home: different audience, different lifecycle, different cadence.
// Folding an operator screen into it would couple two things that have no
// reason to change together.
//
// The contract, and it is the whole point of the package.
//
//  1. Every row comes back on every response, with a STATUS the server
//     decided. A row that vanished when it passed would make the list length
//     mean something, and a reader could not tell "checked and fine" from
//     "not checked at all".
//
//  2. There are THREE statuses, not two. `unknown` is a first-class answer for
//     a check that cannot run, and it always carries a reason. The billing row
//     is the standing example: invoices and payment methods are in-memory
//     repositories in production, so green would be a lie and red a false
//     alarm. "We cannot look" is a server FACT the screen renders, never a
//     client guess.
//
//  3. Fail-soft per row. One dead source renders ITS row unknown, names itself
//     in part_errors and sets partial. It never blanks the screen: an operator
//     who loses six good answers because a seventh service is down has been
//     given an outage instead of information.
//
// Shape mirrors the medashboard partial / part_errors / known_gaps trio so the
// two aggregators fail the same way and a reader learns it once.
package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Status is a readiness row's verdict.
type Status string

const (
	// StatusOK: checked, and nothing to do.
	StatusOK Status = "ok"
	// StatusAttention: checked, and it needs work. This is the only status
	// that can produce a next action.
	StatusAttention Status = "attention"
	// StatusUnknown: the check could not run. NOT actionable, because an
	// operator cannot act on an answer we do not have. Always carries a
	// reason naming why.
	StatusUnknown Status = "unknown"
)

// DefaultPerCallTimeout bounds each downstream read.
const DefaultPerCallTimeout = 4 * time.Second

// Config carries the downstream bases. An empty URL disables its parts, which
// is a DEPLOYMENT fact declared in known_gaps, not a per-request failure: the
// two have different fixes and conflating them sends an operator hunting an
// outage that is really a missing environment variable.
type Config struct {
	// TenancyURL serves the tenant record (GET /api/v1/tenants/me) and the
	// add-on entitlement selections. It does NOT serve the tenant hierarchy:
	// that read is gRPC-only (Tenancy/GetTenantHierarchy), so the rows it
	// would feed declare a known_gap.
	TenancyURL string
	// IdentityURL serves the IdP providers and the member roster.
	IdentityURL string
	// PerCallTimeout caps each downstream call.
	PerCallTimeout time.Duration
}

// ApplyDefaults fills the per-call timeout when zero.
func (c *Config) ApplyDefaults() {
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultPerCallTimeout
	}
}

// AuthCtx is the caller identity forwarded downstream as mesh headers.
type AuthCtx struct {
	TenantID string
	GCID     string
	Roles    []string
	Bearer   string
}

// Response is the marshalled aggregate plus its HTTP status.
type Response struct {
	Status int
	Body   []byte
}

// Aggregator fans the readiness reads out in parallel.
type Aggregator struct {
	cfg    Config
	client *http.Client
}

// New constructs an Aggregator, or nil when NO downstream is configured so the
// caller can skip the route entirely rather than mount an endpoint that can
// only ever answer "unknown" for everything.
func New(cfg Config) *Aggregator {
	cfg.ApplyDefaults()
	if strings.TrimSpace(cfg.TenancyURL) == "" && strings.TrimSpace(cfg.IdentityURL) == "" {
		return nil
	}
	return &Aggregator{cfg: cfg, client: &http.Client{Timeout: cfg.PerCallTimeout}}
}

// readinessRowDTO is one check on the wire.
//
// Reason is populated for every non-ok row. An `unknown` with no reason tells
// an operator nothing at all, which is worse than the check not existing,
// because it looks like an answer.
type readinessRowDTO struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Count is the number the row is about, when it has one (atoms,
	// administrators, organisations). Omitted rather than zero when the row
	// counts nothing, so a zero always means zero.
	Count *int `json:"count,omitempty"`
}

type readinessDTO struct {
	Rows []readinessRowDTO `json:"rows"`
	// NextAction names the first thing to do, or is empty when nothing needs
	// attention. Unknown rows never produce one.
	NextAction string            `json:"next_action,omitempty"`
	Partial    bool              `json:"partial"`
	PartErrors map[string]string `json:"part_errors,omitempty"`
	KnownGaps  map[string]string `json:"known_gaps,omitempty"`
}

type callResult struct {
	status int
	body   []byte
	err    error
}

func (cr callResult) ok() bool { return cr.err == nil && cr.status >= 200 && cr.status < 300 }

// downstreamErrCode classifies a failure for part_errors. Mirrors the
// medashboard vocabulary so a reader learns one set of codes.
func downstreamErrCode(cr callResult) string {
	switch {
	case cr.err != nil:
		return "upstream_unavailable"
	case cr.status >= 500:
		return "upstream_5xx"
	case cr.status >= 400:
		return "upstream_4xx"
	default:
		return "upstream_unexpected"
	}
}

func (a *Aggregator) call(ctx context.Context, urlStr string, auth AuthCtx) callResult {
	if strings.TrimSpace(urlStr) == "" {
		return callResult{err: fmt.Errorf("readiness: empty url")}
	}
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PerCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, urlStr, nil)
	if err != nil {
		return callResult{err: err}
	}
	req.Header.Set("X-Tenant-Id", auth.TenantID)
	req.Header.Set("gcid", auth.GCID)
	if len(auth.Roles) > 0 {
		req.Header.Set("x-mesh-user-roles", strings.Join(auth.Roles, ","))
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", auth.Bearer)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return callResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return callResult{status: resp.StatusCode, err: err}
	}
	return callResult{status: resp.StatusCode, body: body}
}

// --- downstream shapes, only the fields this aggregate reads ---

type hierarchyWire struct {
	Children []struct {
		TenantID   string `json:"tenant_id"`
		TenantName string `json:"tenant_name"`
		UserCount  int64  `json:"user_count"`
		AtomCount  int64  `json:"atom_count"`
	} `json:"children"`
}

type tenantWire struct {
	Branding struct {
		PrimaryColorHex string `json:"primary_color_hex"`
		LogoURL         string `json:"logo_url"`
	} `json:"branding"`
	WizardCompletedAt *string `json:"wizard_completed_at"`
}

type idpWire struct {
	Items []struct {
		ProviderType string `json:"provider_type"`
	} `json:"items"`
}

// entitlementsWire is the tenancy entitlements projection: one row per
// active add_on_subscriptions row, with the add_ons.code bridged in.
//
// CONTRACT PIN, and it was wrong from E2 part 1 until 2026-09-02. The GRANT
// endpoint this row reads spells the field `addon_code` (chora-tenancy
// internal/adapter/http/handlers.go, entitlementDTO, which calls the name a
// contract pin for the frontend; chora-web pins the same spelling in
// core/services/feature-flags.model.ts). `add_on_code` belongs to the setup
// wizard's INTENT endpoint, GET /api/v1/tenants/me/addons, which also sends a
// `subscriptions` envelope rather than `items`. Reading the intent spelling
// out of the grant envelope matched neither, so every code decoded empty.
//
// Pointer, not string, because the two empties are different facts. Absent
// means no row carried the field at all, which is a wire we do not recognise.
// Present and empty is a legacy add_ons row whose code predates tenancy
// migration 0021. See featuresRow.
type entitlementsWire struct {
	Items []struct {
		AddOnCode *string `json:"addon_code"`
	} `json:"items"`
}

type membersWire struct {
	Items []struct {
		GCID  string   `json:"gcid"`
		Roles []string `json:"roles"`
	} `json:"items"`
}

// errHierarchyNotHTTP marks the tenant-hierarchy read as unavailable over
// HTTP. chora-tenancy serves the direct-children hierarchy ONLY over the
// Tenancy/GetTenantHierarchy gRPC RPC (tenancy-admin.yaml
// §/api/v1/tenancy/tenants/current/hierarchy documents that the gateway
// proxies it over gRPC); there is no HTTP route for it. This HTTP-only
// aggregator therefore cannot read it, so the two rows it feeds report
// unknown with a reason rather than a fabricated count.
var errHierarchyNotHTTP = errors.New("readiness: tenant hierarchy is served over gRPC, not HTTP")

// GetReadiness reads every source in parallel and renders one row per check.
func (a *Aggregator) GetReadiness(ctx context.Context, auth AuthCtx) (Response, error) {
	var (
		wg        sync.WaitGroup
		hierCR    callResult
		tenantCR  callResult
		idpCR     callResult
		addonsCR  callResult
		membersCR callResult
	)
	// The hierarchy is gRPC-only; no HTTP call is made. The two rows it feeds
	// (organisation, content) render unknown via errHierarchyNotHTTP.
	hierCR = callResult{err: errHierarchyNotHTTP}
	wg.Add(4)
	go func() {
		defer wg.Done()
		tenantCR = a.call(ctx, a.cfg.TenancyURL+"/api/v1/tenants/me", auth)
	}()
	go func() {
		defer wg.Done()
		// The ENTITLEMENT read, not the wizard's selections. See featuresRow
		// for why this endpoint is the least-wrong source available today.
		addonsCR = a.call(ctx, a.cfg.TenancyURL+"/api/tenants/"+auth.TenantID+"/entitlements", auth)
	}()
	go func() {
		defer wg.Done()
		idpCR = a.call(ctx, a.cfg.IdentityURL+"/api/v1/tenants/me/idp-providers", auth)
	}()
	go func() {
		defer wg.Done()
		membersCR = a.call(ctx, a.cfg.IdentityURL+"/api/v1/admin/tenant-members", auth)
	}()
	wg.Wait()

	dto := readinessDTO{
		PartErrors: map[string]string{},
		KnownGaps:  map[string]string{},
	}

	// Every builder returns a fully-formed row, so the row set is fixed and a
	// missing source changes a row's STATUS rather than its existence.
	rows := []readinessRowDTO{
		a.organisationRow(hierCR, &dto),
		a.brandingRow(tenantCR, &dto),
		a.featuresRow(addonsCR, &dto),
		a.signInRow(idpCR, &dto),
		a.administratorsRow(membersCR, &dto),
		a.contentRow(hierCR, &dto),
		a.setupRow(tenantCR, &dto),
		billingRow(),
	}
	dto.Rows = rows
	dto.NextAction = nextAction(rows)
	dto.Partial = len(dto.PartErrors) > 0
	if len(dto.PartErrors) == 0 {
		dto.PartErrors = nil
	}
	if len(dto.KnownGaps) == 0 {
		dto.KnownGaps = nil
	}

	body, err := json.Marshal(dto)
	if err != nil {
		return Response{}, fmt.Errorf("readiness: marshal: %w", err)
	}
	return Response{Status: http.StatusOK, Body: body}, nil
}

// unknownRow builds the shape a failed or unconfigured source produces, and
// records the failure in the right bucket: a request failure is a part_error,
// an unset URL is a known_gap. They need different fixes.
func (a *Aggregator) unknownRow(key, label string, cr callResult, dto *readinessDTO) readinessRowDTO {
	if errors.Is(cr.err, errHierarchyNotHTTP) {
		// A protocol mismatch, not a request failure: the hierarchy is served
		// over gRPC, so no amount of retrying this HTTP call will answer it.
		// Declaring it a known_gap keeps it out of part_errors (which would
		// otherwise mark the whole response partial for a source that was
		// never reachable this way).
		dto.KnownGaps[key] = "tenant_hierarchy_is_read_over_grpc_not_http"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "This deployment reads the tenant hierarchy over gRPC; this HTTP check cannot reach it."}
	}
	if strings.TrimSpace(cr.err.Error()) == "readiness: empty url" {
		dto.KnownGaps[key] = "downstream_not_configured"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "This deployment has no address configured for the service that answers this check."}
	}
	dto.PartErrors[key] = downstreamErrCode(cr)
	return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
		Reason: "We could not reach the service that answers this check. The other rows are still accurate."}
}

// failedRow routes a non-ok call to the right unknown shape.
func (a *Aggregator) failedRow(key, label string, cr callResult, dto *readinessDTO) (readinessRowDTO, bool) {
	if cr.ok() {
		return readinessRowDTO{}, false
	}
	if cr.err != nil {
		return a.unknownRow(key, label, cr, dto), true
	}
	dto.PartErrors[key] = downstreamErrCode(cr)
	return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
		Reason: "The service that answers this check returned an error. The other rows are still accurate."}, true
}

func intPtr(n int) *int { return &n }

func (a *Aggregator) organisationRow(cr callResult, dto *readinessDTO) readinessRowDTO {
	const key, label = "organisation", "Organisations"
	if row, failed := a.failedRow(key, label, cr, dto); failed {
		return row
	}
	var h hierarchyWire
	if err := json.Unmarshal(cr.body, &h); err != nil {
		dto.PartErrors[key] = "decode_failed"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "The organisation list came back in a shape we do not recognise."}
	}
	n := len(h.Children)
	if n == 0 {
		return readinessRowDTO{Key: key, Label: label, Status: StatusAttention,
			Detail: "None yet. This is the next thing to do.", Count: intPtr(0)}
	}
	return readinessRowDTO{Key: key, Label: label, Status: StatusOK,
		Detail: fmt.Sprintf("%d live.", n), Count: intPtr(n)}
}

func (a *Aggregator) contentRow(cr callResult, dto *readinessDTO) readinessRowDTO {
	const key, label = "content", "Content"
	if row, failed := a.failedRow(key, label, cr, dto); failed {
		return row
	}
	var h hierarchyWire
	if err := json.Unmarshal(cr.body, &h); err != nil {
		dto.PartErrors[key] = "decode_failed"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "The content count came back in a shape we do not recognise."}
	}
	var atoms int
	for _, c := range h.Children {
		atoms += int(c.AtomCount)
	}
	// The projection is forward-only: atoms created before tenancy migration
	// 0027 went live are not counted. Declared rather than silently under-
	// reported, because a low number here would otherwise read as missing
	// content rather than as a known limit of the counter.
	dto.KnownGaps["content"] = "atom_count_is_forward_only_from_migration_0027"
	if atoms == 0 {
		return readinessRowDTO{Key: key, Label: label, Status: StatusAttention,
			Detail: "No atoms yet, so there is nothing for a learner to do.", Count: intPtr(0)}
	}
	return readinessRowDTO{Key: key, Label: label, Status: StatusOK,
		Detail: fmt.Sprintf("%d atoms.", atoms), Count: intPtr(atoms)}
}

func (a *Aggregator) brandingRow(cr callResult, dto *readinessDTO) readinessRowDTO {
	const key, label = "branding", "Branding"
	if row, failed := a.failedRow(key, label, cr, dto); failed {
		return row
	}
	var tw tenantWire
	if err := json.Unmarshal(cr.body, &tw); err != nil {
		dto.PartErrors[key] = "decode_failed"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "The tenant record came back in a shape we do not recognise."}
	}
	if strings.TrimSpace(tw.Branding.PrimaryColorHex) == "" && strings.TrimSpace(tw.Branding.LogoURL) == "" {
		return readinessRowDTO{Key: key, Label: label, Status: StatusAttention,
			Detail: "Still on the default look."}
	}
	return readinessRowDTO{Key: key, Label: label, Status: StatusOK, Detail: "Set."}
}

func (a *Aggregator) setupRow(cr callResult, dto *readinessDTO) readinessRowDTO {
	const key, label = "setup", "Setup"
	if row, failed := a.failedRow(key, label, cr, dto); failed {
		return row
	}
	var tw tenantWire
	if err := json.Unmarshal(cr.body, &tw); err != nil {
		dto.PartErrors[key] = "decode_failed"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "The tenant record came back in a shape we do not recognise."}
	}
	if tw.WizardCompletedAt == nil || strings.TrimSpace(*tw.WizardCompletedAt) == "" {
		return readinessRowDTO{Key: key, Label: label, Status: StatusAttention,
			Detail: "Setup has not been run through to the end."}
	}
	return readinessRowDTO{Key: key, Label: label, Status: StatusOK, Detail: "Completed."}
}

func (a *Aggregator) signInRow(cr callResult, dto *readinessDTO) readinessRowDTO {
	const key, label = "signIn", "Sign-in"
	if row, failed := a.failedRow(key, label, cr, dto); failed {
		return row
	}
	var iw idpWire
	if err := json.Unmarshal(cr.body, &iw); err != nil {
		dto.PartErrors[key] = "decode_failed"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "The sign-in providers came back in a shape we do not recognise."}
	}
	n := len(iw.Items)
	if n == 0 {
		return readinessRowDTO{Key: key, Label: label, Status: StatusAttention,
			Detail: "No provider yet. People can still sign in with the platform default.", Count: intPtr(0)}
	}
	return readinessRowDTO{Key: key, Label: label, Status: StatusOK,
		Detail: fmt.Sprintf("%d connected.", n), Count: intPtr(n)}
}

func (a *Aggregator) administratorsRow(cr callResult, dto *readinessDTO) readinessRowDTO {
	const key, label = "administrators", "Administrators"
	if row, failed := a.failedRow(key, label, cr, dto); failed {
		return row
	}
	var mw membersWire
	if err := json.Unmarshal(cr.body, &mw); err != nil {
		dto.PartErrors[key] = "decode_failed"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "The member roster came back in a shape we do not recognise."}
	}
	n := 0
	for _, m := range mw.Items {
		for _, r := range m.Roles {
			if strings.EqualFold(strings.TrimSpace(r), "admin") {
				n++
				break
			}
		}
	}
	if n == 0 {
		return readinessRowDTO{Key: key, Label: label, Status: StatusAttention,
			Detail: "None yet, and this one matters most.", Count: intPtr(0)}
	}
	return readinessRowDTO{Key: key, Label: label, Status: StatusOK,
		Detail: fmt.Sprintf("%d.", n), Count: intPtr(n)}
}

// featuresRow reports whether this organisation actually holds features.
//
// THE SOURCE IS THE LEAST-WRONG ONE AVAILABLE, and that is worth stating
// plainly rather than implying more.
//
// The durable record is `add_on_subscriptions`. There is no endpoint that
// reads it. `pg.LegacyEntitlementStore.ListActiveByTenant` does exactly that
// query, but at boot `newRegistryEntitlementStore` WRAPS it and serves the
// in-memory `addon.SubscriptionRegistry` instead; that wrapper's own comment
// records that `/api/feature-flags` and `GET /api/tenants/{id}/entitlements`
// both flow through it. The registry is the table as of the last hydration
// plus whatever this process has written since.
//
// So this row reads the entitlements endpoint and is honest about what that
// means. The failure direction is deliberate: a tenant whose durable rows
// have not yet reached the registry reads as "declared, not yet entitled",
// which UNDER-claims. An operator told to confirm a plan that is already
// granted loses a minute; an operator told a plan is granted when it is not
// discovers it when a learner cannot use the feature. Only one of those is
// acceptable, so the row never reports ok on absent evidence.
//
// baselineOnly is the discriminator: the baseline plan is granted to every
// tenant at creation, so its presence alone proves nothing about the chosen
// add-ons.
func (a *Aggregator) featuresRow(cr callResult, dto *readinessDTO) readinessRowDTO {
	const key, label = "features", "Plan and features"
	if row, failed := a.failedRow(key, label, cr, dto); failed {
		return row
	}
	var ew entitlementsWire
	if err := json.Unmarshal(cr.body, &ew); err != nil {
		dto.PartErrors[key] = "decode_failed"
		return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
			Reason: "The entitlement list came back in a shape we do not recognise."}
	}
	// NARROWED 2026-09-02 by chora-tenancy dfab160a3, not closed. The read now
	// falls through to add_on_subscriptions when the registry has NEVER seen
	// the tenant, so a tenant created after the serving pod booted is read
	// durably. A tenant the registry HAS seen is still answered from the boot
	// snapshot, so a grant made elsewhere after that hydration is still
	// invisible here. Stating the old blanket gap would now be wrong, and
	// stating no gap would be worse.
	dto.KnownGaps[key] = "entitlements_read_serves_the_in_memory_registry_for_tenants_it_hydrated_at_boot_and_add_on_subscriptions_only_for_tenants_it_never_saw"

	beyondBaseline := 0
	for _, it := range ew.Items {
		// No row carried the field the grant endpoint is contracted to send.
		// An unrecognised shape is an ERROR, not an empty result, so it lands
		// in part_errors and the response reports itself partial. Answering
		// "only the baseline plan" here is what the pre-fix code did, and it
		// stated a fact the response does not support.
		if it.AddOnCode == nil {
			dto.PartErrors[key] = "entitlement_rows_carried_no_addon_code_field"
			return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
				Reason: "The entitlement list carried no addon_code, so this cannot say what the plan includes. " +
					"Rows in this shape come from the setup wizard's selection endpoint, which is not the granted plan."}
		}
		// A grant that carries no catalogue code is a different fact: the
		// response is well formed, so nothing failed and nothing belongs in
		// part_errors, but the row still cannot name what is granted. Treating
		// it as the baseline would hide a real add-on.
		if strings.TrimSpace(*it.AddOnCode) == "" {
			return readinessRowDTO{Key: key, Label: label, Status: StatusUnknown,
				Reason: "A granted add-on has no catalogue code, so this cannot say what the plan includes."}
		}
		if !isBaselineCode(*it.AddOnCode) {
			beyondBaseline++
		}
	}
	if beyondBaseline == 0 {
		return readinessRowDTO{
			Key: key, Label: label, Status: StatusAttention, Count: intPtr(0),
			Detail: "Only the baseline plan.",
			Reason: "Declared, not yet entitled: nothing beyond the baseline is granted, or the grant has not reached this process yet.",
		}
	}
	return readinessRowDTO{Key: key, Label: label, Status: StatusOK,
		Detail: fmt.Sprintf("%d beyond the baseline.", beyondBaseline), Count: intPtr(beyondBaseline)}
}

// isBaselineCode reports the always-on baseline plan under either of its two
// names. The row is stored as `core` and the running catalogue calls the same
// plan `base`; tenancy migration 0035 and pg.TranslateAddOnCode both encode
// that bridge, and a reader here has to honour it or a baseline-only tenant
// would look entitled under one name and not the other.
// An empty code is NOT handled here. featuresRow answers it before the call,
// because "we cannot read the codes" and "this tenant holds only the always-on
// plan" are different answers and this predicate can only give one of them.
func isBaselineCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "core", "base":
		return true
	default:
		return false
	}
}

// billingRow is the standing example of a check that cannot run.
//
// Invoices and payment methods are in-memory repositories in production, which
// is why the H+ billing screen errors under Invoice history. Any verdict would
// be invented, so the row says so and says why, as a server fact rather than a
// client guess.
func billingRow() readinessRowDTO {
	return readinessRowDTO{
		Key: "billing", Label: "Billing", Status: StatusUnknown,
		Reason: "Chora cannot check this yet: invoices and payment methods are held in memory, not stored, so any answer would be invented.",
	}
}

// nextAction names the first row that needs work, in the fixed row order.
//
// UNKNOWN rows are skipped deliberately: an operator cannot act on an answer
// we do not have, and telling them to "fix billing" when the truth is that we
// cannot look would send them somewhere there is nothing to do.
func nextAction(rows []readinessRowDTO) string {
	for _, r := range rows {
		if r.Status == StatusAttention {
			return actionFor(r.Key)
		}
	}
	return ""
}

func actionFor(key string) string {
	switch key {
	case "organisation":
		return "Create an organisation."
	case "branding":
		return "Set the branding for this organisation."
	case "features":
		return "Confirm the plan for this organisation."
	case "signIn":
		return "Connect a sign-in provider."
	case "administrators":
		return "Invite an administrator."
	case "content":
		return "Add the first content."
	case "setup":
		return "Finish setup."
	default:
		return ""
	}
}

// LoadConfigFromEnv reads the downstream bases from the environment, per
// feedback_no_inline_config. An unset variable disables its parts, which the
// aggregate declares as a known_gap rather than reporting as an outage.
func LoadConfigFromEnv() Config {
	c := Config{
		TenancyURL:  os.Getenv("SVC_TENANCY_URL"),
		IdentityURL: os.Getenv("SVC_IDENTITY_URL"),
	}
	c.ApplyDefaults()
	return c
}
