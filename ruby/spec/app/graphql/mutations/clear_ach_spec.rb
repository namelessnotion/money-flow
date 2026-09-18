# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Mutations::ClearAch do
  let(:ach) { create(:ach_transaction) }
  let(:clear_now) { instance_double(Services::Ach::ClearNow) }

  def execute(query)
    MoneyFlowSchema.execute(query, variables: { id: ach.id }).to_h
  end

  before { allow(Services::Ach::ClearNow).to receive(:new).and_return(clear_now) }

  it 'clears the deposit now and answers with its projected state' do
    create(:transaction_projection, aggregate_id: ach.id, state: 'completed')
    allow(clear_now).to receive(:call).and_return(ach)

    result = execute('mutation($id: ID!) { clearAch(achTransactionId: $id) { achTransaction { id state } } }')

    expect(result.dig('data', 'clearAch', 'achTransaction')).to eq('id' => ach.id, 'state' => 'COMPLETED')
    expect(clear_now).to have_received(:call).with(ach_transaction_id: ach.id)
  end

  it 'answers a deposit that cannot be cleared yet as a GraphQL error' do
    allow(clear_now).to receive(:call).and_raise(Services::Ach::NotClearable, 'not completed yet')

    result = execute('mutation($id: ID!) { clearAch(achTransactionId: $id) { achTransaction { id } } }')

    expect(result.dig('errors', 0, 'message')).to eq('not completed yet')
  end
end
