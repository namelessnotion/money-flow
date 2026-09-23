# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Draw do
  subject(:service) { described_class.new(gateway: securities_gateway) }

  let(:world) { securities_world(principal_minor_units: 100_000) }
  let(:security) { world.security }

  before do
    stub_go_securities_happy_path
    security.update(offering_transaction_id: SecureRandom.uuid_v7)
    create(:transaction_projection, aggregate_id: security.offering_transaction_id, state: 'completed')
  end

  # Sells `amount` of the Security to an investor, as the read model sees it.
  def subscribe(amount, investor: world.investor)
    subscription = create(:subscription, security: security, investor: investor, amount_minor_units: amount)
    create(:transaction_projection, aggregate_id: subscription.id, state: 'completed')
  end

  def sent_request
    request = nil
    expect(transaction_client).to have_received(:start_initializing_transaction) { |req| request = req }
    request
  end

  context 'when the Security is fully subscribed' do
    before { subscribe(100_000) }

    it "moves the whole escrow to the Borrower's cleared cash" do
      drawn = service.call(security_id: security.id)
      leg = sent_request.transfers[drawn.draw_transfer_id]

      expect(sent_request.id).to eq(drawn.draw_transaction_id)
      expect(sent_request.factory_name).to eq('security_draw')
      expect(leg.amount.minor_units).to eq(100_000)
      expect(leg.to_wallet_id).to eq(world.wallet_of(world.borrower, 'cleared_cash'))
    end

    it 'derives the ids from the Security, so asking twice converges rather than drawing twice' do
      drawn = service.call(security_id: security.id)

      expect(drawn.draw_transaction_id).to eq(Services::DetId.for("security:#{security.id}:draw"))
      expect(drawn.draw_transfer_id).to eq(Services::DetId.for("security:#{security.id}:draw:transfer"))
    end

    it 'records the ids before calling Go, so a crash cannot lose them' do
      recorded = nil
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        recorded = Models::Security[security.id].draw_transaction_id
        transaction_initialized(req.id)
      end

      drawn = service.call(security_id: security.id)

      expect(recorded).to eq(drawn.draw_transaction_id)
    end

    it 'asks Go to accept it and nothing more' do
      # A short escrow is refused at accept time — the leg is a root, so the
      # pre-flight sees it — and that is the answer an operator gets. Whether
      # the money actually moved arrives through the projection, because since
      # the async cutover it is the orchestrator that moves it.
      drawn = service.call(security_id: security.id)

      expect(transaction_client).to have_received(:start_initializing_transaction) do |req|
        expect(req.id).to eq(drawn.draw_transaction_id)
      end
      expect(transaction_client).not_to have_received(:get_transaction_state)
    end

    it "raises Go's reason when the escrow is short, which the pre-flight catches" do
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        transaction_rejected(req.id, 'wallet "escrow" has insufficient Token capacity: 1 USD short')
      end

      expect { service.call(security_id: security.id) }
        .to raise_error(Services::Securities::Refused, /insufficient Token capacity/)
    end

    it 'refuses to draw a second time once the read model has seen the first' do
      drawn = service.call(security_id: security.id)
      create(:transaction_projection, aggregate_id: drawn.draw_transaction_id, state: 'completed')

      expect { service.call(security_id: security.id) }
        .to raise_error(Services::Securities::NotDrawable, /already been drawn/)
    end
  end

  describe 'what it refuses' do
    it 'refuses a Security that is not fully subscribed' do
      subscribe(99_999)

      expect { service.call(security_id: security.id) }
        .to raise_error(Services::Securities::NotDrawable, /only a fully subscribed Security draws/)
    end

    it 'refuses a Security nobody has bought into' do
      expect { service.call(security_id: security.id) }
        .to raise_error(Services::Securities::NotDrawable, /offering/)
    end

    it 'refuses an unknown security' do
      expect { service.call(security_id: SecureRandom.uuid_v7) }
        .to raise_error(Services::Securities::NotFound, /no security/)
    end

    it 'never reaches Go when it refuses' do
      expect { service.call(security_id: security.id) }.to raise_error(Services::Securities::NotDrawable)
      expect(transaction_client).not_to have_received(:start_initializing_transaction)
    end
  end
end
