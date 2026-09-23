# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Purchase do
  subject(:service) { described_class.new(gateway: securities_gateway) }

  let(:world) { securities_world(principal_minor_units: 100_000) }

  # An offering the read model has seen open.
  before do
    world.security.update(offering_transaction_id: SecureRandom.uuid_v7)
    create(:transaction_projection, aggregate_id: world.security.offering_transaction_id, state: 'completed')
  end

  def buy(amount: 25_000, investor: world.investor)
    service.call(security_id: world.security.id, investor_entity_id: investor.id, amount_minor_units: amount)
  end

  def sent_request
    request = nil
    expect(transaction_client).to have_received(:start_initializing_transaction) { |req| request = req }
    request
  end

  context 'when Go accepts it' do
    before { stub_go_securities_happy_path }

    it 'records the intent under the ids it asked Go to run' do
      subscription = buy

      expect(sent_request.id).to eq(subscription.id)
      expect(sent_request.transfers.keys)
        .to contain_exactly(subscription.claim_transfer_id, subscription.money_transfer_id)
      expect(subscription.amount_minor_units).to eq(25_000)
      expect(subscription.currency).to eq('USD')
    end

    it 'records the intent before calling Go, so a crash cannot lose the ids' do
      rows_when_started = nil
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        rows_when_started = Models::Subscription.count
        transaction_initialized(req.id)
      end

      buy

      expect(rows_when_started).to eq(Models::Subscription.count)
    end

    it 'asks Go to accept the Transaction and nothing more' do
      # Running it belongs to the orchestrator now, and the outcome reaches Ruby
      # through the projection. Asking Go how it went would get back
      # "initialized" and tell a caller nothing.
      subscription = buy

      expect(transaction_client).to have_received(:start_initializing_transaction) do |req|
        expect(req.id).to eq(subscription.id)
      end
      expect(transaction_client).not_to have_received(:get_transaction_state)
    end

    it 'lets one Investor buy into the same Security more than once' do
      first = buy(amount: 10_000)
      second = buy(amount: 15_000)

      expect(second.id).not_to eq(first.id)
      expect(Models::Subscription.where(security_id: world.security.id).count).to eq(2)
    end
  end

  context 'when the offering is oversubscribed' do
    let(:reason) do
      # Go's own wording, from transfer.selectSourceTokens.
      'transfer "claim": wallet "supply" has insufficient Token capacity: 2500 USD short'
    end

    before do
      stub_go_securities_happy_path
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        transaction_rejected(req.id, reason)
      end
    end

    it 'raises the reason, framed so it is clear nothing moved' do
      expect { buy }.to raise_error(Services::Securities::Refused, /before any money moved.*insufficient Token/)
    end

    it 'is refused synchronously, because the claim leg is the DAG root' do
      # Oversubscription is the one refusal a caller still learns here: the
      # claim leg is ready at time zero, so Go's accept-time pre-flight sees it
      # and rejects before writing anything.
      expect { buy }.to raise_error(Services::Securities::Refused)
      expect(transaction_client).not_to have_received(:get_transaction_state)
    end

    it 'keeps the intent, whose projection will show it rejected' do
      expect { buy }.to raise_error(Services::Securities::Refused)
      expect(Models::Subscription.where(security_id: world.security.id).count).to eq(1)
    end
  end

  context 'when the Investor has too little cleared cash' do
    # The executable statement of the trade-off PurchaseShape documents, and of
    # what the async cutover did to it. The money leg sits behind a dependency
    # edge, so Go's accept-time pre-flight never sees it: Go accepts the
    # Transaction, and only later — in the orchestrator — dispatches the claim
    # leg, fails the money leg and reverses the claim leg.
    #
    # Nothing here can learn that. It used to, by resuming; now the Transaction
    # is merely accepted when this returns. So the Investor gets a Subscription
    # back and finds out from its projected state, through
    # Services::Securities::Stage, the same way they find out everything else.
    before { stub_go_securities_happy_path }

    it 'returns the Subscription rather than raising, because the outcome is not known yet' do
      subscription = buy

      expect(subscription).to be_a(Models::Subscription)
      expect(transaction_client).not_to have_received(:get_transaction_state)
    end

    it 'records the intent, which is what the projection will attach the rollback to' do
      subscription = buy

      expect(Models::Subscription[subscription.id]).not_to be_nil
    end
  end

  describe 'what it refuses before reaching Go' do
    before { stub_go_securities_happy_path }

    it 'refuses more than the read model says is left' do
      buy(amount: 90_000)
      create(:transaction_projection, aggregate_id: Models::Subscription.last.id, state: 'completed')

      expect { buy(amount: 20_000) }
        .to raise_error(Services::Securities::Oversubscribed, /has 10000 left/)
    end

    it 'refuses a non-positive amount' do
      expect { buy(amount: 0) }.to raise_error(Services::Securities::InvalidAmount, /positive/)
    end

    it 'refuses an entity that is not an investor' do
      expect { buy(investor: world.borrower) }
        .to raise_error(Services::Securities::WrongRole, /not an investor/)
    end

    it 'refuses an unknown security' do
      expect do
        service.call(security_id: SecureRandom.uuid_v7, investor_entity_id: world.investor.id,
                     amount_minor_units: 100)
      end.to raise_error(Services::Securities::NotFound, /no security/)
    end

    it 'refuses a Security whose supply the read model has never seen minted' do
      unopened = create(:security, issuer: world.issuer, borrower: world.borrower)
      open_security_wallets(world.issuer, unopened)

      expect do
        service.call(security_id: unopened.id, investor_entity_id: world.investor.id, amount_minor_units: 100)
      end.to raise_error(Services::Securities::Oversubscribed, /not open for subscription/)
    end
  end

  context 'when Go is briefly unavailable' do
    let(:ids_sent) { [] }

    before do
      stub_go_securities_happy_path
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        ids_sent << req.id
        raise Faraday::ConnectionFailed, 'connection reset' if ids_sent.size == 1

        transaction_initialized(req.id)
      end
    end

    it 'retries with the same Transaction id, so Go converges rather than buying twice' do
      subscription = buy

      expect(ids_sent).to eq([subscription.id, subscription.id])
      expect(Models::Subscription.where(security_id: world.security.id).count).to eq(1)
    end
  end
end
