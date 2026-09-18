# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Mutations::InitiateAch do
  let(:entity) { create(:entity) }
  let(:ach) { create(:ach_transaction, entity: entity) }
  let(:service) { instance_double(Services::Ach::Initiate) }

  def execute(field)
    MoneyFlowSchema.execute(<<~GRAPHQL, variables: { entityId: entity.id.to_s }).to_h
      mutation($entityId: ID!) {
        #{field}(entityId: $entityId, amountMinorUnits: "10000") {
          achTransaction { id direction amountMinorUnits currency state }
        }
      }
    GRAPHQL
  end

  before { allow(Services::Ach::Initiate).to receive(:new).and_return(service) }

  it 'initiates a deposit and answers with the ACH Transaction' do
    allow(service).to receive(:call).and_return(ach)

    result = execute('initiateAchDeposit')

    expect(result['errors']).to be_nil
    expect(result.dig('data', 'initiateAchDeposit', 'achTransaction')).to eq(
      'id' => ach.id, 'direction' => 'DEPOSIT', 'amountMinorUnits' => '10000', 'currency' => 'USD', 'state' => nil
    )
    expect(service).to have_received(:call) do |request:|
      expect([request.entity_id, request.direction, request.amount_minor_units])
        .to eq([entity.id, Types::Enums::AchDirection::Deposit, 10_000])
    end
  end

  it 'initiates a withdrawal' do
    allow(service).to receive(:call).and_return(ach)

    execute('initiateAchWithdrawal')

    expect(service).to have_received(:call) do |request:|
      expect(request.direction).to eq(Types::Enums::AchDirection::Withdrawal)
    end
  end

  it 'answers a refusal as a GraphQL error' do
    allow(service).to receive(:call).and_raise(Services::Ach::Refused, 'wallet policy')

    result = execute('initiateAchDeposit')

    expect(result.dig('errors', 0, 'message')).to eq('wallet policy')
    expect(result.dig('data', 'initiateAchDeposit')).to be_nil
  end

  it 'lets an outage escape as one' do
    allow(service).to receive(:call).and_raise(Services::Ach::Unavailable, 'go down')

    expect { execute('initiateAchDeposit') }.to raise_error(Services::Ach::Unavailable)
  end
end
