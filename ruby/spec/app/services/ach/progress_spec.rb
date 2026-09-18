# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Progress do
  # Takes the direction and each state as they are persisted; a leg's state
  # is a Transfer's, the others a Transaction's.
  def steps(direction: 'deposit', submitted: true, **states)
    projected = states.to_h do |key, value|
      [key, (key.end_with?('leg_state') ? Types::Enums::TransferState : Types::Enums::TransactionState)
        .try_deserialize(value)]
    end
    seen = Services::Ach::Progress::Snapshot.new(direction: Types::Enums::AchDirection.deserialize(direction),
                                                 submitted: submitted, **projected)
    described_class.of(seen).to_h { |step| [step.name.serialize, step.status.serialize] }
  end

  it 'walks a deposit through initiation, submission, settlement, completion and clearing' do
    expect(steps.keys).to eq(%w[initiation submission settlement completion clearing])
  end

  it 'funds a withdrawal before submitting it, and has no clearing step, since a withdrawal never clears' do
    expect(steps(direction: 'withdrawal').keys).to eq(%w[initiation funding submission settlement completion])
  end

  it 'funds a withdrawal once its cleared cash has moved to bank control' do
    expect(steps(direction: 'withdrawal', submitted: false, state: 'started', shadow_leg_state: 'committed'))
      .to eq('initiation' => 'done', 'funding' => 'done', 'submission' => 'waiting',
             'settlement' => 'waiting', 'completion' => 'waiting')
  end

  it 'fails funding when cleared cash is short, before anything was submitted, and shows the rollback done' do
    expect(steps(direction: 'withdrawal', submitted: false, state: 'rolled_back', shadow_leg_state: 'rejected'))
      .to eq('initiation' => 'done', 'funding' => 'failed', 'submission' => 'skipped',
             'settlement' => 'skipped', 'completion' => 'skipped', 'rollback' => 'done')
  end

  it 'waits on the rollback of a returned withdrawal until its funding is reversed' do
    expect(steps(direction: 'withdrawal', state: 'rollback_started', real_leg_state: 'cancelled',
                 shadow_leg_state: 'committed'))
      .to eq('initiation' => 'done', 'funding' => 'done', 'submission' => 'done',
             'settlement' => 'failed', 'completion' => 'skipped', 'rollback' => 'waiting')
  end

  it 'waits on everything before Go or the provider has answered' do
    expect(steps(submitted: false).values.uniq).to eq(['waiting'])
  end

  it 'counts a submitted entry as initiated even while the projection lags' do
    expect(steps(submitted: true)).to include('initiation' => 'done', 'submission' => 'done',
                                              'settlement' => 'waiting')
  end

  it 'waits on settlement while the real leg is pending' do
    expect(steps(state: 'started', real_leg_state: 'pending')).to eq(
      'initiation' => 'done', 'submission' => 'done', 'settlement' => 'waiting',
      'completion' => 'waiting', 'clearing' => 'waiting'
    )
  end

  it 'settles when the real leg posts, and completes when the Transaction does' do
    expect(steps(state: 'completed', real_leg_state: 'committed')).to eq(
      'initiation' => 'done', 'submission' => 'done', 'settlement' => 'done',
      'completion' => 'done', 'clearing' => 'waiting'
    )
  end

  it 'is done once the clearing Transaction completes' do
    expect(steps(state: 'completed', real_leg_state: 'committed', clearing_state: 'completed'))
      .to include('clearing' => 'done')
  end

  it 'fails clearing when the clearing Transaction is rolled back' do
    expect(steps(state: 'completed', real_leg_state: 'committed', clearing_state: 'rolled_back'))
      .to include('clearing' => 'failed')
  end

  it 'fails settlement on a return, skips what would have followed, and shows the rollback' do
    expect(steps(state: 'rolled_back', real_leg_state: 'cancelled')).to eq(
      'initiation' => 'done', 'submission' => 'done', 'settlement' => 'failed',
      'completion' => 'skipped', 'clearing' => 'skipped', 'rollback' => 'done'
    )
  end

  it 'fails submission when the provider refused the entry' do
    expect(steps(submitted: false, state: 'rolled_back', real_leg_state: 'cancelled')).to eq(
      'initiation' => 'done', 'submission' => 'failed', 'settlement' => 'skipped',
      'completion' => 'skipped', 'clearing' => 'skipped', 'rollback' => 'done'
    )
  end

  it 'fails completion, and waits on the rollback, while the rollback is under way' do
    expect(steps(state: 'rollback_started', real_leg_state: 'committed'))
      .to include('completion' => 'failed', 'rollback' => 'waiting')
  end

  it 'fails the rollback when Go could not undo what had moved' do
    expect(steps(state: 'rollback_failed', real_leg_state: 'cancelled')).to include('rollback' => 'failed')
  end

  it 'has no rollback step while nothing is being rolled back' do
    expect(steps(state: 'completed', real_leg_state: 'committed').keys).not_to include('rollback')
    expect(steps(submitted: false, state: 'rejected').keys).not_to include('rollback')
  end

  it 'fails initiation when Go rejected the Transaction' do
    expect(steps(submitted: false, state: 'rejected')).to eq(
      'initiation' => 'failed', 'submission' => 'skipped', 'settlement' => 'skipped',
      'completion' => 'skipped', 'clearing' => 'skipped'
    )
  end

  it 'answers with Step structs' do
    seen = Services::Ach::Progress::Snapshot.new(direction: Types::Enums::AchDirection::Withdrawal, submitted: false,
                                                 state: nil, real_leg_state: nil, shadow_leg_state: nil,
                                                 clearing_state: nil)
    expect(described_class.of(seen).first)
      .to be_a(Services::Ach::Progress::Step)
      .and have_attributes(name: Services::Ach::Progress::Name::Initiation,
                           status: Services::Ach::Progress::Status::Waiting)
  end
end
