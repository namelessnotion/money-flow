# frozen_string_literal: true

require 'spec_helper'

# Why a Borrower's Repayment can't take the cash out from under an ACH
# withdrawal they've funded (spec/alloy/ledger.als,
# FundedWithdrawalCanBePaid; namelessnotion/money_flow#7). Both shapes
# debit the Borrower's cleared cash and cash with separate legs. They're
# safe together because both debit the cleared side first, so the
# Borrower's cash always covers their cleared cash (ruby/docs/adr/0010).
# Whichever Transaction takes the cleared cash first has the cash behind
# it. The other is refused before it touches the cash.
#
# The interleaving is replayed against Go's ledger in
# go/internal/transaction/two_sides_race_test.go.
RSpec.describe Services::Securities::RepaymentShape do
  context 'with an ACH withdrawal by the same Borrower beside it' do
    let(:world) { securities_world }
    let(:borrower_wallets) { wallet_uuids_of(world.borrower) }
    let(:ids) { %i[repayment real shadow].to_h { |leg| [leg, SecureRandom.uuid_v7] } }

    let(:repayment) do
      described_class
        .for(security: world.security, accounts: world.accounts, transfer_id: ids[:repayment])
        .start_request(transaction_id: SecureRandom.uuid_v7, amount_minor_units: 10_000)
    end

    let(:withdrawal) do
      Services::Ach::TransactionShape
        .for(direction: Types::Enums::AchDirection::Withdrawal,
             accounts: Models::Account.where(entity_id: world.borrower.id).all,
             real_transfer_id: ids[:real], shadow_transfer_id: ids[:shadow])
        .start_request(transaction_id: SecureRandom.uuid_v7, amount_minor_units: 10_000)
    end

    # Whether the leg debiting the Borrower's `later` wallet waits, directly or
    # through other legs, for the one debiting their `earlier` wallet.
    def debit_waits?(request, later:, earlier:)
      waits_for(request, debiting(request, later)).include?(debiting(request, earlier))
    end

    def debiting(request, type)
      request.transfers.values.find { |leg| leg.from_wallet_id == borrower_wallets.fetch(type) }.id
    end

    # Every leg `id` waits for, directly or not.
    def waits_for(request, id)
      parents = request.transfer_dependency[id]&.transfer_id.to_a
      parents + parents.flat_map { |parent| waits_for(request, parent) }
    end

    it "debits the Borrower's cleared cash before their cash in both" do
      expect(debit_waits?(repayment, later: 'cash', earlier: 'cleared_cash')).to be true
      expect(debit_waits?(withdrawal, later: 'cash', earlier: 'cleared_cash')).to be true
    end

    it "makes the Repayment's cash leg wait for its money leg" do
      cash_leg = Services::Securities::Leg.cash_id_for(ids[:repayment])

      expect(repayment.transfers[cash_leg].from_wallet_id).to eq(borrower_wallets['cash'])
      expect(repayment.transfer_dependency[cash_leg].transfer_id).to eq([ids[:repayment]])
    end
  end
end
