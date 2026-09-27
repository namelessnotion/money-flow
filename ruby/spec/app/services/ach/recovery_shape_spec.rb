# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::RecoveryShape do
  let(:entity) { create_provisioned_entity }
  let(:wallets) { wallet_uuids_of(entity) }
  let(:shape) do
    wallets
    described_class.for(entity_id: entity.id, sequence: 3, accounts: Models::Account.where(entity_id: entity.id).all)
  end
  let(:request) { shape.start_request(amount_minor_units: 4_000) }

  def id_for(suffix) = Services::DetId.for("#{entity.id}:recovery:3#{suffix}")

  def transfer(suffix) = request.transfers[id_for(suffix)] || raise("no #{suffix} leg")

  it 'derives its ids from the entity and the sequence' do
    expect(request.id).to eq(id_for(''))
    expect(shape.transaction_id).to eq(request.id)
  end

  it "collects from the entity's cleared cash into bank control, then from its cash into its Receivable" do
    expect([transfer(':cleared').from_wallet_id, transfer(':cleared').to_wallet_id])
      .to eq([wallets['cleared_cash'], wallets['bank_control']])
    expect([transfer(':cash').from_wallet_id, transfer(':cash').to_wallet_id])
      .to eq([wallets['cash'], wallets['receivable']])
  end

  it 'moves the same amount on both legs, staging and minting nothing' do
    expect(request.transfers.values.map { |leg| [leg.amount.minor_units, leg.stage, leg.mint_source] })
      .to eq([[4_000, false, false], [4_000, false, false]])
  end

  # The entity pays, so its cleared cash leaves first (ruby/docs/adr/0010).
  it 'runs the cleared-cash leg first' do
    expect(request.transfer_dependency.to_h.transform_values { |ids| ids[:transfer_id] })
      .to eq(id_for(':cash') => [id_for(':cleared')])
  end

  it 'names its factory' do
    expect([request.factory_name, request.factory_version]).to eq(%w[receivable_recovery 1])
  end
end
