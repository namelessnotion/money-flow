# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Stage do
  let(:name) { Services::Securities::Stage::Name }
  let(:status_of) { Services::Securities::Stage::Status }
  let(:state) { Types::Enums::TransactionState }

  # A Security nobody has touched yet: nothing minted, nothing sold.
  def untouched
    { offering_state: nil, draw_state: nil, principal_minor_units: 100_000,
      subscribed_minor_units: 0, outstanding_principal_minor_units: 0, repaid_anything: false }
  end

  def snapshot(**overrides)
    attributes = untouched.merge(overrides)
    Services::Securities::Stage::Snapshot.new(**attributes)
  end

  def status(name, **overrides)
    described_class.of(snapshot(**overrides)).find { |step| step.name == name }.status
  end

  it 'walks the phases in order' do
    expect(described_class.of(snapshot).map(&:name))
      .to eq([name::Offering, name::Funded, name::Drawn, name::Repaying, name::Repaid])
  end

  describe 'offering' do
    it 'waits until the read model has seen the supply mint' do
      expect(status(name::Offering)).to eq(status_of::Waiting)
    end

    it 'is done once the mint completed' do
      expect(status(name::Offering, offering_state: state::Completed)).to eq(status_of::Done)
    end

    it 'fails when Go refused it' do
      expect(status(name::Offering, offering_state: state::Rejected)).to eq(status_of::Failed)
    end

    it 'skips every later phase once it failed, because none of them will run' do
      steps = described_class.of(snapshot(offering_state: state::Rejected,
                                          subscribed_minor_units: 100_000))

      expect(steps.drop(1).map(&:status)).to all(eq(status_of::Skipped))
    end
  end

  describe 'funded' do
    it 'waits while claims are still for sale' do
      expect(status(name::Funded, subscribed_minor_units: 99_999)).to eq(status_of::Waiting)
    end

    it 'is done once every claim is sold' do
      expect(status(name::Funded, subscribed_minor_units: 100_000)).to eq(status_of::Done)
    end
  end

  describe 'drawn' do
    it 'is done once the draw completed' do
      expect(status(name::Drawn, draw_state: state::Completed)).to eq(status_of::Done)
    end

    it 'fails when the draw rolled back' do
      expect(status(name::Drawn, draw_state: state::RolledBack)).to eq(status_of::Failed)
    end
  end

  describe 'repaid' do
    it 'is not repaid merely because nobody bought in' do
      # A Security nobody subscribed to also has nothing outstanding, and
      # calling that repaid would be a lie about a loan that never happened.
      expect(status(name::Repaid, outstanding_principal_minor_units: 0)).to eq(status_of::Waiting)
    end

    it 'waits while any principal is still owed' do
      expect(status(name::Repaid, repaid_anything: true, outstanding_principal_minor_units: 1))
        .to eq(status_of::Waiting)
    end

    it 'is done once the Borrower has paid and nothing is outstanding' do
      expect(status(name::Repaid, repaid_anything: true, outstanding_principal_minor_units: 0))
        .to eq(status_of::Done)
    end
  end

  describe '.current' do
    it 'reads as an offering before anything has happened' do
      expect(described_class.current(snapshot)).to eq(name::Offering)
    end

    it 'names the furthest phase reached' do
      reached = snapshot(offering_state: state::Completed, subscribed_minor_units: 100_000,
                         draw_state: state::Completed, repaid_anything: true,
                         outstanding_principal_minor_units: 50_000)

      expect(described_class.current(reached)).to eq(name::Repaying)
    end

    it 'names the last phase once it is fully repaid' do
      done = snapshot(offering_state: state::Completed, subscribed_minor_units: 100_000,
                      draw_state: state::Completed, repaid_anything: true,
                      outstanding_principal_minor_units: 0)

      expect(described_class.current(done)).to eq(name::Repaid)
    end
  end

  describe '.snapshot_of' do
    it 'reads what Ruby last saw of a real Security' do
      world = securities_world(principal_minor_units: 100_000)
      security = world.security
      security.update(offering_transaction_id: SecureRandom.uuid_v7)
      create(:transaction_projection, aggregate_id: security.offering_transaction_id, state: 'completed')

      taken = described_class.snapshot_of(security.refresh)

      expect(taken.offering_state).to eq(state::Completed)
      expect(taken.draw_state).to be_nil
      expect(taken.principal_minor_units).to eq(100_000)
      expect(taken.repaid_anything).to be false
    end
  end
end
