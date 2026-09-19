# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Initiate do
  subject(:service) { described_class.new(gateway: go_gateway, provider: provider) }

  let(:provider) { Services::Ach::FakeProvider.new }
  let(:entity) { create(:entity) }
  let(:request) do
    InitiateAchRequest.new(entity_id: entity.id, direction: Types::Enums::AchDirection::Deposit,
                           amount_minor_units: 10_000)
  end

  before do
    Types::Enums::AccountType.each_value { |type| create(:account, entity: entity, type: type.serialize) }
  end

  context 'when Go and the provider accept it' do
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

    it 'submits the entry to the provider and confirms the staged real leg' do
      ach = service.call(request: request)

      expect(transfer_client).to have_received(:confirm_staged_transfer) do |req|
        expect(req.id).to eq(ach.real_transfer_id)
      end
      expect(ach.reload.provider_reference).to eq("fake-ach-#{ach.id}")
    end
  end

  context 'when Go rejects the Transaction for an underfunded withdrawal' do
    # A withdrawal funds itself from cleared cash first; without enough, Go's
    # own accept/reject decision refuses the whole Transaction before any
    # event exists (go/docs/adr/0004) — the same shape as any other
    # structural rejection, just discovered a little later than a DAG error.
    let(:request) do
      InitiateAchRequest.new(entity_id: entity.id, direction: Types::Enums::AchDirection::Withdrawal,
                             amount_minor_units: 10_000)
    end

    before do
      allow(provider).to receive(:submit).and_call_original
      stub_go_happy_path
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        rejected = Transaction::V1::TransactionRejected.new(
          id: req.id,
          reason: 'transfer "shadow" (wallet "w"): wallet "w" has insufficient Token capacity: 10000 USD short'
        )
        twirp_ok(Transaction::V1::StartInitializingTransactionResponse.new(id: req.id, transaction_rejected: rejected))
      end
    end

    it 'raises the reason without submitting the entry, so no money leaves' do
      expect { service.call(request: request) }
        .to raise_error(Services::Ach::Refused, /before any money moved.*insufficient Token capacity/)

      expect(provider).not_to have_received(:submit)
      expect(transfer_client).not_to have_received(:confirm_staged_transfer)
    end

    it 'never asks Go to resume — the rejection from start_transaction is already definitive' do
      expect { service.call(request: request) }.to raise_error(Services::Ach::Refused)

      expect(transaction_client).not_to have_received(:resume_transaction)
    end

    it 'keeps the intent, whose projection will show it rejected' do
      expect { service.call(request: request) }.to raise_error(Services::Ach::Refused)

      expect(Models::AchTransaction.where(entity_id: entity.id).count).to eq(1)
    end
  end

  it 'asks Go how the Transaction stands before submitting the entry' do
    stub_go_happy_path

    ach = service.call(request: request)

    expect(transaction_client).to have_received(:resume_transaction) { |req| expect(req.id).to eq(ach.id) }
  end

  context 'when Go rejects the Transaction' do
    before do
      stub_go_happy_path
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        rejected = Transaction::V1::TransactionRejected.new(id: req.id, reason: 'policy')
        twirp_ok(Transaction::V1::StartInitializingTransactionResponse.new(id: req.id, transaction_rejected: rejected))
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

  context 'when the provider refuses the entry' do
    let(:provider) do
      Class.new(Services::Ach::FakeProvider) do
        def submit(_entry) = raise(Services::Ach::Provider::SubmissionFailed, 'account closed')
      end.new
    end

    before { stub_go_happy_path }

    it 'resumes the Transaction so Go rolls it back' do
      expect { service.call(request: request) }.to raise_error(Services::Ach::Refused)

      # Once to check it is running before submission, once after cancelling.
      expect(transaction_client).to have_received(:resume_transaction).twice
    end

    it 'cancels the staged real leg instead of confirming it' do
      expect { service.call(request: request) }.to raise_error(Services::Ach::Refused, /account closed/)

      ach = Models::AchTransaction.first(entity_id: entity.id)
      expect(transfer_client).to have_received(:cancel_staged_transfer) do |req|
        expect(req.id).to eq(ach.real_transfer_id)
        expect(req.reason).to include('account closed')
      end
      expect(transfer_client).not_to have_received(:confirm_staged_transfer)
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
