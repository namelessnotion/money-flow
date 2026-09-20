# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::DisbursementShape do
  let(:world) { securities_world }
  let(:payout_id) { SecureRandom.uuid_v7 }
  let(:retirement_id) { SecureRandom.uuid_v7 }

  def request_for(principal:, interest:, retirement: retirement_id)
    described_class
      .for(security: world.security, investor_entity_id: world.investor.id, accounts: world.accounts,
           payout_transfer_id: payout_id, retirement_transfer_id: retirement)
      .start_request(transaction_id: SecureRandom.uuid_v7,
                     principal_minor_units: principal, interest_minor_units: interest)
  end

  context 'when the repayment returns principal as well as interest' do
    let(:request) { request_for(principal: 40_000, interest: 3_000) }

    it 'names the factory that built it' do
      expect([request.factory_name, request.factory_version]).to eq(%w[security_disbursement 1])
    end

    it "retires the repaid claim back into the Issuer's control wallet" do
      leg = request.transfers[retirement_id]

      expect(leg.from_wallet_id).to eq(world.wallet_of(world.investor, 'investment'))
      expect(leg.to_wallet_id).to eq(world.wallet_of(world.issuer, 'issuer_control'))
      expect(leg.amount.minor_units).to eq(40_000)
    end

    it 'pays principal and interest together into cleared cash' do
      leg = request.transfers[payout_id]

      expect(leg.from_wallet_id).to eq(world.security_wallet('security_repayment'))
      expect(leg.to_wallet_id).to eq(world.wallet_of(world.investor, 'cleared_cash'))
      expect(leg.amount.minor_units).to eq(43_000)
    end

    it 'retires the claim before paying for it, so no holder is paid principal they do not hold' do
      # investment permits neither direction, so its Token carries
      # debits_must_not_exceed_credits: putting the retirement leg at the root
      # means an over-payment of principal is refused before any money leaves
      # the repayment wallet. It is also the only leg Go pre-flights.
      expect(request.transfer_dependency.keys).to eq([payout_id])
      expect(request.transfer_dependency[payout_id].transfer_id).to eq([retirement_id])
    end

    it 'stages nothing and mints nothing' do
      [payout_id, retirement_id].each do |id|
        expect(request.transfers[id].stage).to be false
        expect(request.transfers[id].mint_source).to be false
      end
    end
  end

  context 'when the repayment is interest only' do
    let(:request) { request_for(principal: 0, interest: 2_500, retirement: nil) }

    it 'omits the retirement leg entirely, because Go refuses a zero-amount Transfer' do
      # Worse than refuses: a zero amount is a transport error the Transaction
      # saga logs and swallows, so the RPC still answers TransactionInitialized
      # and the Transaction sits in Started forever with nothing to explain it.
      expect(request.transfers.keys).to eq([payout_id])
    end

    it 'makes the payout leg the root, so it is the one Go pre-flights' do
      expect(request.transfer_dependency.keys).to be_empty
    end

    it 'pays the interest' do
      expect(request.transfers[payout_id].amount.minor_units).to eq(2_500)
    end
  end

  describe 'the leg-iff-principal invariant' do
    it 'refuses principal with no leg to retire it' do
      expect { request_for(principal: 100, interest: 0, retirement: nil) }
        .to raise_error(Services::Securities::InvalidAmount, /needs a retirement leg/)
    end

    it 'refuses a retirement leg that would move nothing' do
      expect { request_for(principal: 0, interest: 100) }
        .to raise_error(Services::Securities::InvalidAmount, /must not carry a retirement leg/)
    end

    it 'refuses a disbursement of nothing at all' do
      expect { request_for(principal: 0, interest: 0, retirement: nil) }
        .to raise_error(Services::Securities::InvalidAmount, /nothing has no leg/)
    end
  end
end
