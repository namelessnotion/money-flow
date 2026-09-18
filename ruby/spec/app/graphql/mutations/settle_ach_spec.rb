# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Mutations::SettleAch do
  let(:ach) { create(:ach_transaction) }
  let(:settle) { instance_double(Services::Ach::Settle) }

  def execute(query)
    MoneyFlowSchema.execute(query, variables: { id: ach.id }).to_h
  end

  before { allow(Services::Ach::Settle).to receive(:new).and_return(settle) }

  it 'settles the ACH Transaction and answers with its projected state' do
    create(:transaction_projection, aggregate_id: ach.id, state: 'started')
    allow(settle).to receive(:call).and_return(ach)

    result = execute('mutation($id: ID!) { settleAch(achTransactionId: $id) { achTransaction { id state } } }')

    expect(result.dig('data', 'settleAch', 'achTransaction')).to eq('id' => ach.id, 'state' => 'STARTED')
    expect(settle).to have_received(:call).with(ach_transaction_id: ach.id)
  end

  it 'answers an unknown id as a GraphQL error' do
    allow(settle).to receive(:call).and_raise(Services::Ach::NotFound, 'no ACH transaction')

    result = execute('mutation($id: ID!) { settleAch(achTransactionId: $id) { achTransaction { id } } }')

    expect(result.dig('errors', 0, 'message')).to eq('no ACH transaction')
  end
end
