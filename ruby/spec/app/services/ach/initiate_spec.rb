# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Initiate do
  subject(:service) { described_class.new(gateway: go_gateway) }

  let(:entity) { create_provisioned_entity }
  let(:request) do
    InitiateAchRequest.new(entity_id: entity.id, direction: Types::Enums::AchDirection::Deposit,
                           amount_minor_units: 10_000)
  end

  context 'when Go accepts the Transaction' do
    before { stub_go_happy_path }

    it 'records the intent under the ids it asked Go to run' do
      ach = service.call(request: request)

      sent = nil
      expect(transaction_client).to have_received(:start_initializing_transaction) { |req| sent = req }
      expect(sent.id).to eq(ach.id)
      expect(sent.transfers.keys).to contain_exactly(ach.real_transfer_id, ach.shadow_transfer_id)
      expect([ach.direction, ach.amount_minor_units, ach.currency]).to eq(['deposit', 10_000, 'USD'])
    end

    it 'records the intent before calling Go, so a crash cannot lose the ids' do
      rows_when_started = nil
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        rows_when_started = Models::AchTransaction.where(id: req.id).count
        transaction_initialized(req.id)
      end

      service.call(request: request)

      expect(rows_when_started).to eq(1)
    end

    # The cutover, stated where it is most consequential. Accepting the
    # Transaction is the whole of this service now: the funding leg has not run
    # when it returns, so there is nothing to be sure of and nothing may reach
    # the provider. Services::Ach::SubmitDue does that, once the ledger has
    # actually moved (ruby/docs/adr/0008).
    it 'hands nothing to the provider, and does not pretend to know how it went' do
      ach = service.call(request: request)

      expect(ach.provider_reference).to be_nil
      expect(transfer_client).not_to have_received(:confirm_staged_transfer)
      expect(transaction_client).not_to have_received(:get_transaction_state)
    end
  end

  context 'when Go rejects the Transaction for an underfunded withdrawal' do
    # A withdrawal funds itself from cleared cash first, and that leg is ready
    # at time zero — so Go's accept-time pre-flight sees it and refuses the
    # whole Transaction before any event exists (go/docs/adr/0004). This is the
    # one money-safety case that is still answered synchronously, and it is the
    # common one.
    let(:request) do
      InitiateAchRequest.new(entity_id: entity.id, direction: Types::Enums::AchDirection::Withdrawal,
                             amount_minor_units: 10_000)
    end

    before do
      stub_go_happy_path
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        transaction_rejected(
          req.id,
          'transfer "shadow" (wallet "w"): wallet "w" has insufficient Token capacity: 10000 USD short'
        )
      end
    end

    it 'raises the reason, and nothing is left for the sweep to submit' do
      expect { service.call(request: request) }
        .to raise_error(Services::Ach::Refused, /before any money moved.*insufficient Token capacity/)

      expect(transfer_client).not_to have_received(:confirm_staged_transfer)
    end

    it 'keeps the intent, whose projection will show it rejected' do
      expect { service.call(request: request) }.to raise_error(Services::Ach::Refused)

      expect(Models::AchTransaction.where(entity_id: entity.id).count).to eq(1)
    end
  end

  context 'when Go rejects the Transaction' do
    before do
      stub_go_happy_path
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        transaction_rejected(req.id, 'policy')
      end
    end

    it 'raises the reason, keeping the intent the rejection belongs to' do
      # Wrapped with the same "before any money moved" framing every
      # start_transaction-time rejection gets — accurate for any reason,
      # since nothing has moved yet regardless of why Go refused.
      expect { service.call(request: request) }
        .to raise_error(Services::Ach::Refused, 'refused before any money moved: policy')

      expect(Models::AchTransaction.where(entity_id: entity.id).count).to eq(1)
      expect(transfer_client).not_to have_received(:confirm_staged_transfer)
    end
  end

  context 'when Go is briefly unavailable' do
    it 'retries with the same Transaction id' do
      stub_go_happy_path
      ids = []
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        ids << req.id
        next Twirp::ClientResp.new(error: Twirp::Error.unavailable('later')) if ids.size == 1

        transaction_initialized(req.id)
      end

      service.call(request: request)

      expect(ids.size).to eq(2)
      expect(ids.uniq.size).to eq(1)
    end
  end

  it 'refuses a non-positive amount without writing anything' do
    zero = InitiateAchRequest.new(entity_id: entity.id, direction: Types::Enums::AchDirection::Deposit,
                                  amount_minor_units: 0)

    expect { service.call(request: zero) }.to raise_error(Services::Ach::InvalidAmount)
    expect(Models::AchTransaction.where(entity_id: entity.id)).to be_empty
  end

  it 'refuses an unknown entity' do
    unknown = InitiateAchRequest.new(entity_id: -1, direction: Types::Enums::AchDirection::Deposit,
                                     amount_minor_units: 1)

    expect { service.call(request: unknown) }.to raise_error(Services::Ach::NotFound)
  end
end
