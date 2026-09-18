# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Models::TokenBalanceProjection do
  describe '.totals_for' do
    let(:checking) { SecureRandom.uuid_v7 }
    let(:savings) { SecureRandom.uuid_v7 }

    def token_balance(wallet_uuid, posted, outgoing: 0, incoming: 0)
      create(:token_balance_projection, wallet_uuid: wallet_uuid, posted_minor_units: posted,
                                        pending_outgoing_minor_units: outgoing, pending_incoming_minor_units: incoming)
    end

    it "sums each Wallet's Tokens into one balance per currency" do
      token_balance(checking, 600, outgoing: 50)
      token_balance(checking, -100, incoming: 20)
      token_balance(savings, 400)

      totals = described_class.totals_for([checking, savings])

      expect(totals.fetch(checking)).to eq(
        [Models::AccountBalance.new(currency: 'USD', posted: 500, pending_outgoing: 50, pending_incoming: 20)]
      )
      expect(totals.fetch(savings)).to eq(
        [Models::AccountBalance.new(currency: 'USD', posted: 400, pending_outgoing: 0, pending_incoming: 0)]
      )
    end

    it 'leaves out a Wallet with no Token balances' do
      expect(described_class.totals_for([checking])).to eq({})
    end

    it 'ignores Wallets that were not asked for' do
      create(:token_balance_projection, wallet_uuid: savings, posted_minor_units: 400)

      expect(described_class.totals_for([checking])).to eq({})
    end
  end
end
