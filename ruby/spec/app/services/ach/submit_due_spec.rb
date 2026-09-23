# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::SubmitDue do
  subject(:sweep) { described_class.new(logger: logger, submit: submit) }

  let(:logger) { Logger.new(StringIO.new) }
  let(:provider) { Services::Ach::FakeProvider.new }
  let(:submit) { Services::Ach::Submit.new(gateway: go_gateway, provider: provider) }
  let(:entity) { create_provisioned_entity }

  before do
    stub_go_happy_path
    allow(provider).to receive(:submit).and_call_original
  end

  # An ACH Transaction and whatever the read model has seen of its real leg.
  def ach(direction: 'withdrawal', real_leg: 'staged', submitted: false)
    row = create(:ach_transaction, entity: entity, direction: direction,
                                   provider_reference: submitted ? 'already-sent' : nil)
    create(:transfer_projection, aggregate_id: row.real_transfer_id, state: real_leg) if real_leg
    row
  end

  it 'submits an entry whose real leg the read model has seen staged' do
    row = ach

    result = sweep.call

    expect(result.submitted).to eq([row.id])
    expect(result.failed).to be_empty
    expect(row.reload.provider_reference).to eq("fake-ach-#{row.id}")
  end

  it 'confirms the staged real leg once the provider holds the entry' do
    row = ach

    sweep.call

    expect(transfer_client).to have_received(:confirm_staged_transfer) do |req|
      expect(req.id).to eq(row.real_transfer_id)
    end
  end

  # The guarantee ruby/docs/adr/0004 exists for. A withdrawal's real leg sits
  # behind the dependency edge on its funding leg, so a leg that is not staged
  # is a withdrawal that is not funded — and the read model may lag Go but it
  # can never lead it, so "not staged yet" is never wrong in the dangerous
  # direction.
  it 'leaves an entry whose real leg is not staged alone, so an unfunded withdrawal never reaches the provider' do
    ach(real_leg: 'accepted')

    result = sweep.call

    expect(result.submitted).to be_empty
    expect(provider).not_to have_received(:submit)
  end

  it 'leaves an entry the read model has not seen at all alone' do
    ach(real_leg: nil)

    expect(sweep.call.submitted).to be_empty
    expect(provider).not_to have_received(:submit)
  end

  # The second half of the gate, and the definitive one: the projection
  # establishes a past fact, and only Go can say nothing has gone wrong since.
  it 'asks Go immediately before submitting, and does not submit if it is no longer running' do
    ach
    allow(transaction_client).to receive(:get_transaction_state) do |req|
      transaction_rolled_back(req.id, 'the funding was reversed')
    end

    result = sweep.call

    expect(result.submitted).to be_empty
    expect(result.failed.values.first).to include('the funding was reversed')
    expect(provider).not_to have_received(:submit)
  end

  it 'never submits a second time for an entry the provider already holds' do
    ach(submitted: true)

    sweep.call

    expect(provider).not_to have_received(:submit)
  end

  # The reference is saved before Go is told, so Go can fail in between. The
  # provider then holds an entry whose real leg is still staged, and the next
  # sweep has to finish the confirmation or the network's settlement would
  # find no pending leg to post.
  it 'confirms a staged leg the provider already holds, which a failed confirmation left behind' do
    row = ach
    allow(transfer_client).to receive(:confirm_staged_transfer).and_raise(Faraday::ConnectionFailed, 'down')
    expect(sweep.call.failed.keys).to eq([row.id])

    confirmed = []
    allow(transfer_client).to receive(:confirm_staged_transfer) do |req|
      confirmed << req.id
      transfer_pending(req.id)
    end

    expect(sweep.call.failed).to be_empty
    expect(confirmed).to eq([row.real_transfer_id])
    expect(provider).to have_received(:submit).once
  end

  it 'leaves an entry the provider holds alone once the read model has seen its leg move past staged' do
    ach(real_leg: 'pending', submitted: true)

    sweep.call

    expect(transfer_client).not_to have_received(:confirm_staged_transfer)
  end

  it 'submits a deposit on the same condition, with no branch of its own' do
    # A deposit's real leg is the DAG root and staging is its first step, so
    # the same predicate reads correctly for both directions.
    row = ach(direction: 'deposit')

    expect(sweep.call.submitted).to eq([row.id])
  end

  it 'carries on past one failure and reports which' do
    good = ach
    bad = ach
    allow(provider).to receive(:submit).and_wrap_original do |original, entry|
      raise Services::Ach::Provider::SubmissionFailed, 'account closed' if entry.transaction_id == bad.id

      original.call(entry)
    end

    result = sweep.call

    expect(result.submitted).to eq([good.id])
    expect(result.failed.keys).to eq([bad.id])
  end

  # A refused submission is a return that happened early, so the Transaction is
  # rolled back the same way rather than left with a staged leg nobody will
  # ever confirm.
  it 'cancels the staged real leg when the provider refuses the entry' do
    row = ach
    allow(provider).to receive(:submit).and_raise(Services::Ach::Provider::SubmissionFailed, 'account closed')

    sweep.call

    expect(transfer_client).to have_received(:cancel_staged_transfer) do |req|
      expect(req.id).to eq(row.real_transfer_id)
      expect(req.reason).to include('account closed')
    end
    expect(transfer_client).not_to have_received(:confirm_staged_transfer)
  end

  it 'can be narrowed to one entry, which is what the demonstration mutation uses' do
    wanted = ach
    ach

    expect(sweep.call(ach_transaction_id: wanted.id).submitted).to eq([wanted.id])
  end
end
