# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Types::Enums::AccountType do
  it 'serializes every value with word separators' do
    # The T::Enum default would give "debitcard"/"unclearedcash", and these
    # strings are persisted in accounts.type and sent as a Wallet's name.
    expect(described_class.values.map(&:serialize)).to contain_exactly(
      'bank', 'bank_control', 'debit_card', 'uncleared_cash', 'cleared_cash', 'cash', 'gain', 'loss',
      'investment', 'issuer_control', 'security_supply', 'security_escrow', 'security_repayment'
    )
  end

  describe '#allows' do
    it 'lets only funding instruments cross the platform boundary' do
      expect(described_class::Bank.allows).to eq(:ALLOWS_ONRAMP_AND_OFFRAMP)
      expect(described_class::BankControl.allows).to eq(:ALLOWS_ONRAMP_AND_OFFRAMP)
      expect(described_class::DebitCard.allows).to eq(:ALLOWS_ONRAMP)
    end

    # Not a boundary type, and the one exception to the rule above. Go's
    # validateMintSource refuses mint_source from any wallet narrower than
    # ALLOWS_ONRAMP, and minting supply is the only way claims enter the
    # ledger — so tidying this to ALLOWS_NONE would break every offering, and
    # only at the point one was issued.
    it 'lets an Issuer mint claim supply, which needs onramp' do
      expect(described_class::IssuerControl.allows).to eq(:ALLOWS_ONRAMP_AND_OFFRAMP)
    end

    it "caps a Security's supply by permitting neither direction on it" do
      # ALLOWS_NONE gives the Token debits_must_not_exceed_credits, which is
      # what refuses an oversubscription once the minted supply is exhausted.
      expect(described_class::SecuritySupply.allows).to eq(:ALLOWS_NONE)
      expect(described_class::Investment.allows).to eq(:ALLOWS_NONE)
    end

    it 'permits neither direction for everything else' do
      internal = described_class.values - [
        described_class::Bank, described_class::BankControl, described_class::DebitCard,
        described_class::IssuerControl
      ]
      expect(internal.map(&:allows).uniq).to eq([:ALLOWS_NONE])
    end

    it 'never returns the unspecified value' do
      # The Wallet service rejects an unset policy, so this would fail onboarding.
      expect(described_class.values.map(&:allows)).not_to include(:ALLOWS_UNSPECIFIED)
    end

    # The symbols aren't checked statically, so a typo would only surface when a
    # real request was built. google-protobuf raises RangeError on an unknown
    # enum name, which makes this a real guard rather than a restatement.
    it 'returns a symbol protobuf accepts for every value' do
      types = described_class.values
      types.each do |type|
        expect { Holder::V1::WalletSpec.new(wallet_id: 'w', name: type.serialize, allows: type.allows) }
          .not_to raise_error
      end
    end
  end

  describe '.for_role' do
    it 'opens the accounts every ACH shape moves money through, whatever the role' do
      # Every role banks. Services::Ach::TransactionShape and ClearingShape
      # have to keep resolving for all three.
      ach = [
        described_class::Bank, described_class::BankControl,
        described_class::UnclearedCash, described_class::ClearedCash, described_class::Cash
      ]

      Types::Enums::EntityRole.each_value do |role|
        expect(described_class.for_role(role)).to include(*ach)
      end
    end

    it 'gives an Investor somewhere to hold claims' do
      expect(described_class.for_role(Types::Enums::EntityRole::Investor))
        .to include(described_class::Investment)
    end

    it 'gives an Issuer somewhere to mint claim supply from' do
      expect(described_class.for_role(Types::Enums::EntityRole::Issuer))
        .to include(described_class::IssuerControl)
    end

    it 'gives a Borrower neither' do
      # A Borrower owes the obligation; it neither holds claims nor issues them.
      expect(described_class.for_role(Types::Enums::EntityRole::Borrower))
        .not_to include(described_class::Investment, described_class::IssuerControl)
    end

    it "never opens a Security's own wallets for a role" do
      # Those are opened per Security by Services::Securities::IssueOffering,
      # and accounts_security_scoped_types refuses one with no security_id.
      scoped = [
        described_class::SecuritySupply, described_class::SecurityEscrow, described_class::SecurityRepayment
      ]

      Types::Enums::EntityRole.each_value do |role|
        expect(described_class.for_role(role)).not_to include(*scoped)
      end
    end

    it 'covers every role' do
      expect { Types::Enums::EntityRole.each_value { |role| described_class.for_role(role) } }
        .not_to raise_error
    end

    it 'never opens the same account type twice for one role' do
      Types::Enums::EntityRole.each_value do |role|
        types = described_class.for_role(role)
        expect(types).to eq(types.uniq)
      end
    end
  end
end
