package main

import (
	"context"
	"fmt"
	"uuid"

	holderpb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
)

// entity is one simulated party in the ledger: a Holder with exactly one
// Wallet. name is a simulator-local label for reporting, not anything Go
// itself knows about.
type entity struct {
	name     string
	holderID string
	walletID string
}

// provision establishes a Holder and opens its one Wallet under allows, as
// one all-or-nothing unit. Reserve and every ordinary entity share this,
// differing only in the Allows policy they honestly declare: reserve is the
// only Wallet allowed to onramp, since it is the mint source every entity's
// starting balance is seeded from.
func provision(ctx context.Context, holders holderpb.HolderService, name string, allows sharedpb.Allows) (entity, error) {
	holderID, walletID := uuid.NewV7().String(), uuid.NewV7().String()
	resp, err := holders.Provision(ctx, &holderpb.ProvisionRequest{
		Id: holderID,
		Wallets: []*holderpb.WalletSpec{
			{WalletId: walletID, Name: name, Allows: allows},
		},
	})
	if err != nil {
		return entity{}, fmt.Errorf("provision %s: %w", name, err)
	}
	if rejected := resp.GetHolderProvisionRejected(); rejected != nil {
		return entity{}, fmt.Errorf("provision %s: rejected: %s", name, rejected.GetReason())
	}
	return entity{name: name, holderID: holderID, walletID: walletID}, nil
}

// provisionAll provisions the reserve Holder plus n ordinary entities.
// Sequential rather than concurrent: onboarding is a one-time setup cost
// paid once per run, not the part this tool means to load-test.
func provisionAll(ctx context.Context, holders holderpb.HolderService, n int) (reserve entity, entities []entity, err error) {
	reserve, err = provision(ctx, holders, "reserve", sharedpb.Allows_ALLOWS_ONRAMP)
	if err != nil {
		return entity{}, nil, err
	}

	entities = make([]entity, 0, n)
	for i := 0; i < n; i++ {
		e, err := provision(ctx, holders, fmt.Sprintf("entity-%d", i), sharedpb.Allows_ALLOWS_NONE)
		if err != nil {
			return entity{}, nil, err
		}
		entities = append(entities, e)
	}
	return reserve, entities, nil
}
