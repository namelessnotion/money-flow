# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Positions do
  # "I wrote it to batch" is not evidence that it batches — the only way to
  # know is to count the queries. This is the same recorder the GraphQL specs
  # use (spec/app/graphql/types/query_type_spec.rb).
  def capture_sql
    statements = []
    recorder = Object.new
    %i[info warn error].each { |level| recorder.define_singleton_method(level) { |message| statements << message } }

    DB.loggers << recorder
    yield
    statements
  ensure
    DB.loggers.delete(recorder)
  end

  def security_with_holders(count)
    world = securities_world(principal_minor_units: 100_000)
    count.times do
      subscription = create(:subscription, security: world.security, investor: create_provisioned_entity,
                                           amount_minor_units: 1_000)
      create(:transaction_projection, aggregate_id: subscription.id, state: 'completed')
    end
    world.security
  end

  describe '.for_securities' do
    it 'reads any number of Securities in the same two queries' do
      securities = Array.new(4) { security_with_holders(2) }

      sqls = capture_sql { described_class.for_securities(securities.map(&:id)) }

      expect(sqls.count { |s| s.include?('FROM "subscriptions"') }).to eq(1)
      expect(sqls.count { |s| s.include?('FROM "disbursements"') }).to eq(1)
    end

    it 'keeps each Security\'s holders to itself' do
      first = security_with_holders(2)
      second = security_with_holders(3)

      by_security = described_class.for_securities([first.id, second.id])

      expect(by_security.fetch(first.id).size).to eq(2)
      expect(by_security.fetch(second.id).size).to eq(3)
    end

    it 'leaves out a Security nobody has bought into' do
      empty = securities_world.security

      expect(described_class.for_securities([empty.id])).to be_empty
    end

    it 'asks nothing of the database for no Securities' do
      sqls = capture_sql { expect(described_class.for_securities([])).to eq({}) }

      expect(sqls).to be_empty
    end

    it 'orders every Security\'s holders by entity id' do
      securities = Array.new(2) { security_with_holders(3) }

      described_class.for_securities(securities.map(&:id)).each_value do |positions|
        ids = positions.map(&:investor_entity_id)
        expect(ids).to eq(ids.sort)
      end
    end
  end
end
