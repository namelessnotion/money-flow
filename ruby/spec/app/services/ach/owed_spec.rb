# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Owed do
  let(:entity) { create_provisioned_entity }

  # A deposit that cleared and then came back: a debt return (ruby/docs/adr/0011).
  def debt(amount, late_return_state: nil, entity: self.entity)
    ach = create(:ach_transaction, entity: entity, amount_minor_units: amount, returned_at: Time.now,
                                   return_reason: 'R10', clearing_transaction_id: SecureRandom.uuid_v7)
    return ach unless late_return_state

    ach.update(return_transaction_id: Services::DetId.for("#{ach.id}:return"))
    create(:transaction_projection, aggregate_id: ach.return_transaction_id, state: late_return_state)
    ach
  end

  def recovery(amount, state:, sequence: 1)
    row = Models::ReceivableRecovery.create(
      id: SecureRandom.uuid_v7, entity_id: entity.id, sequence: sequence, amount_minor_units: amount,
      currency: 'USD', cleared_global_seq: 1
    )
    create(:transaction_projection, aggregate_id: row.id, state: state) if state
    row
  end

  def owed = described_class.for(entity.id)

  it 'is nothing for an entity with no late deposit returns' do
    expect([owed.for_gate, owed.for_recovery]).to eq([0, 0])
  end

  # The gate reads Ruby's own rows, so it closes with the notice, before the
  # projection has seen anything.
  it 'gates on a debt from the moment its notice is recorded' do
    debt(10_000)

    expect([owed.for_gate, owed.for_recovery]).to eq([10_000, 0])
  end

  it 'recovers a debt only once its late return has completed' do
    debt(10_000, late_return_state: 'started')
    debt(2_500, late_return_state: 'completed')

    expect([owed.for_gate, owed.for_recovery]).to eq([12_500, 2_500])
  end

  it 'takes off what completed Recoveries collected, and nothing else' do
    debt(10_000, late_return_state: 'completed')
    recovery(4_000, state: 'completed', sequence: 1)
    recovery(1_000, state: 'rolled_back', sequence: 2)
    recovery(500, state: 'started', sequence: 3)
    recovery(250, state: nil, sequence: 4)

    expect([owed.for_gate, owed.for_recovery]).to eq([6_000, 6_000])
  end

  it 'counts neither a clawback nor a withdrawal return, which leave nothing owed' do
    create(:ach_transaction, entity: entity, returned_at: Time.now, return_reason: 'R01')
    create(:ach_transaction, entity: entity, direction: 'withdrawal', returned_at: Time.now, return_reason: 'R02',
                             clearing_transaction_id: nil)

    expect([owed.for_gate, owed.for_recovery]).to eq([0, 0])
  end

  it "counts only the entity's own debts" do
    debt(10_000, late_return_state: 'completed', entity: create_provisioned_entity)

    expect([owed.for_gate, owed.for_recovery]).to eq([0, 0])
  end

  # Token balances reach Ruby on another topic and can lag the Transaction
  # states. Reading the Receivable's balance could see a Recovery complete
  # before its credit, and collect it twice.
  it "never reads the Receivable's balance" do
    debt(10_000, late_return_state: 'completed')
    recovery(4_000, state: 'completed')
    receivable = Models::Account.first(entity_id: entity.id, type: 'receivable')
    create(:token_balance_projection, wallet_uuid: receivable.wallet_uuid, posted_minor_units: -10_000)

    expect(owed.for_recovery).to eq(6_000)
  end

  it 'lists the entities that owe something recoverable' do
    debt(10_000, late_return_state: 'completed')
    repaid = create_provisioned_entity
    debt(1_000, late_return_state: 'completed', entity: repaid)
    Models::ReceivableRecovery.create(id: SecureRandom.uuid_v7, entity_id: repaid.id, sequence: 1,
                                      amount_minor_units: 1_000, currency: 'USD', cleared_global_seq: 1)
                              .then { |row| create(:transaction_projection, aggregate_id: row.id, state: 'completed') }

    expect(described_class.recoverable_entity_ids).to include(entity.id)
    expect(described_class.recoverable_entity_ids).not_to include(repaid.id)
  end
end
