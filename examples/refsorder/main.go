// refsorder demonstrates reusable order references without a database or server.
// All ownership data and caller authorization in this example are test fakes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/refs"
)

const (
	demoTenant   = "tenant-demo"
	merchantID   = "11111111-1111-4111-8111-111111111111"
	accountID    = "22222222-2222-4222-8222-222222222222"
	groupID      = "33333333-3333-4333-8333-333333333333"
	channelID    = "44444444-4444-4444-8444-444444444444"
	otherID      = "55555555-5555-4555-8555-555555555555"
	pspJSON      = `{"merchant_id":"` + merchantID + `","merchant_account_id":"` + accountID + `","channel_group_id":"` + groupID + `","channel_id":"` + channelID + `"}`
	unroutedJSON = `{"merchant_id":"` + merchantID + `","merchant_account_id":"` + accountID + `"}`
)

// Order is the same resource representation in both example projects. A real
// order also has its own amount, currency, status, timestamps and other facts;
// refs is not a replacement for those fields or for TenantID.
type Order struct {
	ID       string      `json:"id"`
	TenantID string      `json:"tenant_id"`
	Refs     refs.Values `json:"refs"`
}

// These specs stand in for declarations in shared, versioned resource contracts.
// The two profiles have distinct resource identities: neither silently redefines
// order.psp_order/v1 based on deployment-time settings.
func pspSchema() (*refs.Schema, error) {
	return refs.NewSchema(refs.Spec{
		Domain: "order", Resource: "psp_order", Version: 1,
		Definitions: []refs.Definition{
			{Key: "merchant_id", Target: "merchant.merchant", Format: refs.FormatUUID, Required: true, Immutable: true},
			{Key: "merchant_account_id", Target: "merchant.account", Format: refs.FormatUUID, Required: true, Immutable: true},
			{Key: "channel_group_id", Target: "channel.group", Format: refs.FormatUUID, Immutable: true},
			{Key: "channel_id", Target: "channel.channel", Format: refs.FormatUUID, Immutable: true},
		},
	})
}

func storeSchema() (*refs.Schema, error) {
	return refs.NewSchema(refs.Spec{
		Domain: "order", Resource: "store_order", Version: 1,
		Definitions: []refs.Definition{
			{Key: "store_id", Target: "retail.store", Format: refs.FormatOpaque, Required: true, Immutable: true},
			{Key: "salesperson_id", Target: "directory.employee", Format: refs.FormatOpaque},
		},
	})
}

// fakeAuthorizedScope represents a scope whose authentication AND order-create
// permission have already been checked. Only a fixture constructs it here. In a
// real application, verified identity and explicit permission checks supply the
// scope; never derive it from refs, request-body tenant_id or unsigned headers.
type fakeAuthorizedScope struct{ tenantID string }

// fakeOwnershipResolver is deliberately not a merchant/channel implementation.
// Real adapters call the owner domains' contracts with trusted delegated scope,
// outside the order transaction. Structural schema validation cannot do this.
type fakeOwnershipResolver struct{}

func (fakeOwnershipResolver) validatePSP(ctx context.Context, scope fakeAuthorizedScope, values refs.Values) error {
	if err := ctx.Err(); err != nil {
		return apperr.Unavailable(err)
	}
	merchant, _ := values.Get("merchant_id")
	account, _ := values.Get("merchant_account_id")
	if scope.tenantID != demoTenant || merchant != merchantID || account != accountID {
		return apperr.PermissionDenied("fake account is not available in this authorized scope")
	}
	group, hasGroup := values.Get("channel_group_id")
	channel, hasChannel := values.Get("channel_id")
	if hasGroup != hasChannel {
		return apperr.InvalidArgument("routing must supply both channel references")
	}
	if hasGroup && (group != groupID || channel != channelID) {
		return apperr.PermissionDenied("fake channel is not available in this authorized scope")
	}
	return nil
}

// preparePSPOrder only constructs a candidate; it does NOT persist an order.
// The real use case would next open its local transaction, write the order and
// refs atomically, and publish its outbox event. Resolving an account before that
// transaction does not make account freezing and order creation atomic.
func preparePSPOrder(ctx context.Context, schema *refs.Schema, scope fakeAuthorizedScope, raw []byte) (Order, error) {
	values, err := schema.DecodeJSON(raw)
	if err != nil {
		return Order{}, err
	}
	if err := (fakeOwnershipResolver{}).validatePSP(ctx, scope, values); err != nil {
		return Order{}, err
	}
	return Order{ID: "order-psp-1", TenantID: scope.tenantID, Refs: values}, nil
}

func run(w io.Writer) error {
	psp, err := pspSchema()
	if err != nil {
		return err
	}
	store, err := storeSchema()
	if err != nil {
		return err
	}
	order, err := preparePSPOrder(context.Background(), psp, fakeAuthorizedScope{tenantID: demoTenant}, []byte(pspJSON))
	if err != nil {
		return err
	}
	storeRefs, err := store.DecodeJSON([]byte(`{"store_id":"store-1","salesperson_id":"employee-1"}`))
	if err != nil {
		return err
	}
	storeOrder := Order{ID: "order-store-1", TenantID: demoTenant, Refs: storeRefs}
	for _, item := range []Order{order, storeOrder} {
		wire, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, string(wire)); err != nil {
			return err
		}
	}

	filter, err := refs.DecodeJSON([]byte(`{"merchant_id":"` + merchantID + `","channel_id":"` + channelID + `"}`))
	if err != nil {
		return err
	}
	if err := psp.ValidateFilter(filter); err != nil {
		return err
	}
	before, err := psp.DecodeJSON([]byte(unroutedJSON))
	if err != nil {
		return err
	}
	if err := psp.ValidateUpdate(before, order.Refs); err != nil {
		return err
	}
	if err := psp.Validate(order.Refs); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "OK: four PSP references, no-merchant store order, partial filter, first routing assignment"); err != nil {
		return err
	}

	changed := order.Refs.Map()
	changed["channel_id"] = otherID
	after, err := refs.NewValues(changed)
	if err != nil {
		return err
	}
	if psp.ValidateUpdate(order.Refs, after) == nil {
		return apperr.Internal(fmt.Errorf("example expected immutable-reference rejection"))
	}
	if _, err := psp.DecodeJSON([]byte(`{"merchant_id":"` + merchantID + `","merchant_id":"` + otherID + `"}`)); err == nil {
		return apperr.Internal(fmt.Errorf("example expected duplicate-key rejection"))
	}
	if _, err := fmt.Fprintln(w, "REJECTED: changing assigned channel; duplicate JSON reference key"); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, "DEMO ONLY: fake authorization/ownership; no database, server, or cross-domain transaction")
	return err
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
