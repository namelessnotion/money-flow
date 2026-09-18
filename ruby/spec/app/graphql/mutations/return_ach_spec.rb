# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Mutations::ReturnAch do
  let(:ach) { create(:ach_transaction) }
  let(:return_service) { instance_double(Services::Ach::Return) }

  before { allow(Services::Ach::Return).to receive(:new).and_return(return_service) }

  it 'returns the ACH Transaction with the given reason' do
    allow(return_service).to receive(:call).and_return(ach)

    result = MoneyFlowSchema.execute(
      'mutation($id: ID!) { returnAch(achTransactionId: $id, reason: "R01") { achTransaction { id } } }',
      variables: { id: ach.id }
    ).to_h

    expect(result['errors']).to be_nil
    expect(result.dig('data', 'returnAch', 'achTransaction', 'id')).to eq(ach.id)
    expect(return_service).to have_received(:call).with(ach_transaction_id: ach.id, reason: 'R01')
  end
end
