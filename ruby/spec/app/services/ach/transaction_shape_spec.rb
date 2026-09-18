# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::TransactionShape do
  let(:entity) { create(:entity) }
  let(:wallets) do
    Types::Enums::AccountType.values.to_h do |type|
      [type.serialize, create(:account, entity: entity, type: type.serialize).wallet_uuid]
    end
  end
  let(:real_id) { SecureRandom.uuid_v7 }
  let(:shadow_id) { SecureRandom.uuid_v7 }

  def accounts = Models::Account.where(entity_id: entity.id).all

  def shape_for(direction)
    wallets # accounts exist before the shape reads them
    described_class.for(
      direction: direction, accounts: accounts, real_transfer_id: real_id, shadow_transfer_id: shadow_id
    )
  end

  def start_request(direction)
    shape_for(direction).start_request(transaction_id: SecureRandom.uuid_v7, amount_minor_units: 10_000)
  end

  describe 'a deposit' do
    let(:request) { start_request(Types::Enums::AchDirection::Deposit) }

    it 'pulls from the bank account into cash on a staged real leg, minting at the source' do
      real = request.transfers[real_id]
      expect([real.from_wallet_id, real.to_wallet_id]).to eq([wallets['bank'], wallets['cash']])
      expect([real.stage, real.mint_source, real.auto_process]).to eq([true, true, true])
    end

    it 'mirrors it from bank control into uncleared cash on the shadow leg' do
      shadow = request.transfers[shadow_id]
      expect([shadow.from_wallet_id, shadow.to_wallet_id]).to eq([wallets['bank_control'], wallets['uncleared_cash']])
      expect([shadow.stage, shadow.mint_source, shadow.auto_process]).to eq([false, true, true])
    end

    it 'holds the shadow leg until the real leg completes' do
      expect(request.transfer_dependency.to_h.transform_values { |ids| ids[:transfer_id] })
        .to eq(shadow_id => [real_id])
    end

    it 'names the factory that built it' do
      expect([request.factory_name, request.factory_version]).to eq(%w[ach_deposit 1])
    end

    it 'moves the same USD amount on both legs' do
      amounts = request.transfers.values.map { |transfer| [transfer.amount.minor_units, transfer.amount.currency] }
      expect(amounts).to eq([[10_000, 'USD'], [10_000, 'USD']])
    end
  end

  describe 'a withdrawal' do
    let(:request) { start_request(Types::Enums::AchDirection::Withdrawal) }

    it 'pushes cash out to the bank account on a staged real leg, minting nothing' do
      real = request.transfers[real_id]
      expect([real.from_wallet_id, real.to_wallet_id]).to eq([wallets['cash'], wallets['bank']])
      expect([real.stage, real.mint_source]).to eq([true, false])
    end

    it 'mirrors it from cleared cash back to bank control on the shadow leg' do
      shadow = request.transfers[shadow_id]
      expect([shadow.from_wallet_id, shadow.to_wallet_id]).to eq([wallets['cleared_cash'], wallets['bank_control']])
      expect([shadow.stage, shadow.mint_source]).to eq([false, false])
    end

    it 'funds the withdrawal from cleared cash before any money leaves: the real leg waits on the shadow leg' do
      expect(request.transfer_dependency.to_h.transform_values { |ids| ids[:transfer_id] })
        .to eq(real_id => [shadow_id])
    end

    it 'names the factory that built it, at the version that funds first' do
      expect([request.factory_name, request.factory_version]).to eq(%w[ach_withdrawal 2])
    end
  end

  it 'refuses an entity missing an account a leg needs' do
    create(:account, entity: entity, type: 'bank')

    expect do
      described_class.for(direction: Types::Enums::AchDirection::Deposit, accounts: accounts,
                          real_transfer_id: real_id, shadow_transfer_id: shadow_id)
    end.to raise_error(Services::Ach::MissingAccount, /cash/)
  end
end
