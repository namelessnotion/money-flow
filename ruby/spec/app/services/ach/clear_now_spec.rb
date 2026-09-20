# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::ClearNow do
  subject(:service) { described_class.new(clear: Services::Ach::Clear.new(gateway: go_gateway)) }

  let(:entity) { create_provisioned_entity }
  let(:ach) { create(:ach_transaction, entity: entity) }

  before { stub_go_happy_path }

  def projected(state)
    create(:transaction_projection, aggregate_id: ach.id, state: state, state_changed_at: Time.now)
  end

  it 'clears a completed deposit without waiting for its return window' do
    projected('completed')

    service.call(ach_transaction_id: ach.id)

    expect(transaction_client).to have_received(:start_initializing_transaction)
    expect(ach.reload.clearing_transaction_id).not_to be_nil
  end

  it 'refuses a deposit whose Transaction has not completed: there is no uncleared cash yet' do
    projected('started')

    expect { service.call(ach_transaction_id: ach.id) }.to raise_error(Services::Ach::NotClearable, /not completed/)
    expect(transaction_client).not_to have_received(:start_initializing_transaction)
  end

  it 'refuses a deposit the read model has not seen yet' do
    expect { service.call(ach_transaction_id: ach.id) }.to raise_error(Services::Ach::NotClearable, /not yet seen/)
  end

  it 'refuses an unknown id' do
    expect { service.call(ach_transaction_id: SecureRandom.uuid_v7) }.to raise_error(Services::Ach::NotFound)
  end
end
