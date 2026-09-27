# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::LateReturnShape do
  let(:entity) { create_provisioned_entity }
  let(:wallets) { wallet_uuids_of(entity) }

  def ach(direction: 'deposit', cleared: false)
    create(:ach_transaction, entity: entity, direction: direction, amount_minor_units: 10_000,
                             returned_at: Time.now, return_reason: 'R10',
                             clearing_transaction_id: cleared ? SecureRandom.uuid_v7 : nil)
  end

  def shape_for(ach)
    wallets # accounts exist before the shape reads them
    described_class.for(ach: ach, accounts: Models::Account.where(entity_id: entity.id).all)
  end

  def request_for(ach) = shape_for(ach).start_request(amount_minor_units: ach.amount_minor_units)

  def leg(request, ach, role)
    request.transfers[Services::DetId.for("#{ach.id}:return:#{role}")] || raise("no #{role} leg")
  end

  def route(transfer) = [transfer.from_wallet_id, transfer.to_wallet_id]

  def flags(transfer) = [transfer.stage, transfer.mint_source]

  def dependencies(request)
    request.transfer_dependency.to_h.transform_values { |ids| ids[:transfer_id] }
  end

  describe 'the form a late return takes' do
    it 'puts a withdrawal back' do
      expect(shape_for(ach(direction: 'withdrawal')).form).to eq(described_class::Form::WithdrawalReturn)
    end

    it 'claws back a deposit whose Clearing was never recorded' do
      expect(shape_for(ach(cleared: false)).form).to eq(described_class::Form::Clawback)
    end

    it 'records a deposit whose Clearing was recorded as owed' do
      expect(shape_for(ach(cleared: true)).form).to eq(described_class::Form::DebtReturn)
    end
  end

  it "derives its ids from the ACH Transaction's" do
    returned = ach
    request = request_for(returned)

    expect(request.id).to eq(Services::DetId.for("#{returned.id}:return"))
    expect(shape_for(returned).transaction_id).to eq(request.id)
  end

  describe 'a withdrawal return' do
    let(:returned) { ach(direction: 'withdrawal') }
    let(:request) { request_for(returned) }

    it 'mints the money back into cash from the bank, then into cleared cash from bank control' do
      expect(route(leg(request, returned, :cash))).to eq([wallets['bank'], wallets['cash']])
      expect(route(leg(request, returned, :cleared))).to eq([wallets['bank_control'], wallets['cleared_cash']])
      expect(request.transfers.values.map { |transfer| flags(transfer) }).to eq([[false, true], [false, true]])
    end

    # The entity is paid, so its cash arrives first (ruby/docs/adr/0010).
    it 'runs the cash leg first' do
      expect(dependencies(request)).to eq(
        Services::DetId.for("#{returned.id}:return:cleared") => [Services::DetId.for("#{returned.id}:return:cash")]
      )
    end

    it 'names its factory' do
      expect([request.factory_name, request.factory_version]).to eq(%w[ach_withdrawal_return 1])
    end
  end

  describe 'a clawback' do
    let(:returned) { ach(cleared: false) }
    let(:request) { request_for(returned) }

    it 'takes the deposit out of uncleared cash, then out of cash to the bank, minting nothing' do
      expect(route(leg(request, returned, :cleared))).to eq([wallets['uncleared_cash'], wallets['bank_control']])
      expect(route(leg(request, returned, :cash))).to eq([wallets['cash'], wallets['bank']])
      expect(request.transfers.values.map { |transfer| flags(transfer) }).to eq([[false, false], [false, false]])
    end

    # The entity pays, so its cleared side leaves first (ruby/docs/adr/0010).
    it 'runs the cleared-side leg first' do
      expect(dependencies(request)).to eq(
        Services::DetId.for("#{returned.id}:return:cash") => [Services::DetId.for("#{returned.id}:return:cleared")]
      )
    end

    it 'names its factory' do
      expect([request.factory_name, request.factory_version]).to eq(%w[ach_deposit_clawback 1])
    end
  end

  describe 'a debt return' do
    let(:returned) { ach(cleared: true) }
    let(:request) { request_for(returned) }

    it 'mints the whole amount out of the receivable to the bank, whatever the entity still holds' do
      owed = leg(request, returned, :receivable)
      expect(request.transfers.size).to eq(1)
      expect(route(owed)).to eq([wallets['receivable'], wallets['bank']])
      expect(flags(owed)).to eq([false, true])
      expect([owed.amount.minor_units, owed.amount.currency]).to eq([10_000, 'USD'])
      expect(dependencies(request)).to be_empty
    end

    it 'names its factory' do
      expect([request.factory_name, request.factory_version]).to eq(%w[ach_deposit_return 1])
    end
  end

  it 'raises MissingAccount for an entity onboarded before receivables' do
    returned = ach(cleared: true)
    wallets
    Models::Account.where(entity_id: entity.id, type: 'receivable').delete

    expect { shape_for(returned) }.to raise_error(Services::Ach::MissingAccount, /receivable/)
  end
end
