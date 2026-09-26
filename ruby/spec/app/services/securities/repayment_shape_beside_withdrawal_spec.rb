# frozen_string_literal: true

require 'spec_helper'

# What lets a Borrower's Repayment and ACH withdrawal each take one side of
# the same money (spec/alloy/ledger.als, FundedWithdrawalCanBePaid). Both
# shapes debit the Borrower's cleared cash and cash with separate legs, and
# nothing orders the Repayment's cash leg against the withdrawal's Funding.
# Funding therefore doesn't reserve the cash its real leg will need.
#
# The race itself happens in Go's ledger, which these specs stub. It's
# replayed step by step in go/internal/transaction/two_sides_race_test.go.
# This spec pins the shapes that make the race possible, so a change to
# either shape is checked against it.
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

    def debits_of(request) = request.transfers.values.map(&:from_wallet_id)

    it "debits the Borrower's cleared cash and cash in both, one leg per side" do
      both_sides = [borrower_wallets['cleared_cash'], borrower_wallets['cash']]

      expect(debits_of(repayment)).to match_array(both_sides)
      expect(debits_of(withdrawal)).to match_array(both_sides)
    end

    it "lets the Repayment's cash leg run at once, ordered after nothing" do
      cash_leg = Services::Securities::Leg.cash_id_for(ids[:repayment])

      expect(repayment.transfers[cash_leg].from_wallet_id).to eq(borrower_wallets['cash'])
      expect(repayment.transfer_dependency.keys).to be_empty
    end

    it "orders the withdrawal's cash debit after its own Funding only, so Funding reserves no cash" do
      dependencies = withdrawal.transfer_dependency.to_h.transform_values { |list| list[:transfer_id] }

      expect(withdrawal.transfers[ids[:shadow]].from_wallet_id).to eq(borrower_wallets['cleared_cash'])
      expect(withdrawal.transfers[ids[:real]].from_wallet_id).to eq(borrower_wallets['cash'])
      expect(dependencies).to eq(ids[:real] => [ids[:shadow]])
    end
  end
end
