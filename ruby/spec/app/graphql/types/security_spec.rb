# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Types::Security do
  let(:world) { securities_world(principal_minor_units: 100_000) }

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

  def execute(query, variables = {})
    MoneyFlowSchema.execute(query, variables: variables).to_h
  end

  def holds(security, amount, investor: create_provisioned_entity)
    subscription = create(:subscription, security: security, investor: investor, amount_minor_units: amount)
    create(:transaction_projection, aggregate_id: subscription.id, state: 'completed')
    investor
  end

  def offered(security)
    security.update(offering_transaction_id: SecureRandom.uuid_v7)
    create(:transaction_projection, aggregate_id: security.offering_transaction_id, state: 'completed')
    security
  end

  describe 'query { security }' do
    let(:query) do
      <<~GRAPHQL
        query($id: ID!) {
          security(id: $id) {
            id name principalMinorUnits annualRateBps termDays currency
            offeringState stage
            subscribedMinorUnits remainingMinorUnits outstandingPrincipalMinorUnits
            stages { name status }
            issuer { id role }
            borrower { id role }
            positions { investorEntityId principalMinorUnits outstandingPrincipalMinorUnits }
          }
        }
      GRAPHQL
    end

    it 'answers with its terms, its projected state, and what it has sold' do
      security = offered(world.security)
      holds(security, 75_000)

      data = execute(query, { 'id' => security.id }).dig('data', 'security')

      expect(data['principalMinorUnits']).to eq('100000')
      expect(data['offeringState']).to eq('COMPLETED')
      expect(data['subscribedMinorUnits']).to eq('75000')
      expect(data['remainingMinorUnits']).to eq('25000')
      expect(data['stage']).to eq('OFFERING')
    end

    it 'reads as funded once every claim is sold' do
      security = offered(world.security)
      holds(security, 100_000)

      data = execute(query, { 'id' => security.id }).dig('data', 'security')

      expect(data['stage']).to eq('FUNDED')
      expect(data['remainingMinorUnits']).to eq('0')
      expect(data['stages']).to include({ 'name' => 'FUNDED', 'status' => 'DONE' })
    end

    def repaid(security, state)
      repayment = create(:repayment, security: security)
      create(:transaction_projection, aggregate_id: repayment.id, state: state) if state
    end

    it 'reads as repaying once a Repayment against it has completed' do
      security = offered(world.security)
      holds(security, 100_000)
      repaid(security, 'completed')

      data = execute(query, { 'id' => security.id }).dig('data', 'security')

      expect(data['stages']).to include({ 'name' => 'REPAYING', 'status' => 'DONE' })
    end

    { 'rejected' => 'rejected', 'not yet seen' => nil }.each do |label, state|
      it "is not repaying on a Repayment that is #{label}, because nothing has arrived" do
        security = offered(world.security)
        holds(security, 100_000)
        repaid(security, state)

        data = execute(query, { 'id' => security.id }).dig('data', 'security')

        expect(data['stages']).to include({ 'name' => 'REPAYING', 'status' => 'WAITING' })
      end
    end

    it 'names both parties' do
      security = offered(world.security)

      data = execute(query, { 'id' => security.id }).dig('data', 'security')

      expect(data.dig('issuer', 'role')).to eq('ISSUER')
      expect(data.dig('borrower', 'role')).to eq('BORROWER')
    end

    it 'answers with each holder’s position' do
      security = offered(world.security)
      alice = holds(security, 60_000)
      holds(security, 40_000)

      positions = execute(query, { 'id' => security.id }).dig('data', 'security', 'positions')

      expect(positions.size).to eq(2)
      expect(positions.first['investorEntityId']).to eq(alice.id.to_s)
      expect(positions.first['principalMinorUnits']).to eq('60000')
    end

    it 'is null for an id that is not a uuid, rather than an error' do
      result = execute(query, { 'id' => 'not-a-uuid' })

      expect(result['errors']).to be_nil
      expect(result.dig('data', 'security')).to be_nil
    end
  end

  describe 'query { securities }' do
    let(:query) do
      <<~GRAPHQL
        query($issuerEntityId: ID) {
          securities(issuerEntityId: $issuerEntityId) {
            nodes {
              id stage subscribedMinorUnits
              positions { investorEntityId }
              issuer { id }
            }
          }
        }
      GRAPHQL
    end

    it 'reads a page of Securities without one Positions query per row' do
      # "I wrote it to batch" is not evidence — count the queries.
      4.times do
        security = offered(securities_world.security)
        2.times { holds(security, 1_000) }
      end

      sqls = capture_sql { execute(query) }

      expect(sqls.count { |s| s.include?('FROM "subscriptions"') }).to eq(1)
      expect(sqls.count { |s| s.include?('FROM "disbursements"') }).to eq(1)
    end

    it 'reads whether each Security has been repaid without one query per row' do
      4.times { offered(securities_world.security) }

      sqls = capture_sql { execute(query) }

      expect(sqls.count { |s| s.include?('FROM "repayments"') }).to eq(1)
    end

    it 'reads every named party in one query, not one per row' do
      3.times { offered(securities_world.security) }

      sqls = capture_sql { execute(query) }

      expect(sqls.count { |s| s.include?('FROM "entities"') }).to eq(1)
    end

    it 'lists only the Securities an Issuer offered' do
      mine = offered(world.security)
      offered(securities_world.security)

      ids = execute(query, { 'issuerEntityId' => world.issuer.id.to_s })
            .dig('data', 'securities', 'nodes').map { |node| node['id'] }

      expect(ids).to eq([mine.id])
    end

    it 'matches nothing for an issuer id that is not a bigint, rather than erroring' do
      offered(world.security)

      result = execute(query, { 'issuerEntityId' => 'nope' })

      expect(result['errors']).to be_nil
      expect(result.dig('data', 'securities', 'nodes')).to be_empty
    end
  end

  describe 'a Security’s subscriptions and repayments' do
    %w[subscriptions repayments].each do |field|
      it "matches nothing in #{field} for a security id that is not a uuid, rather than erroring" do
        result = execute("query { #{field}(securityId: \"not-a-uuid\") { nodes { id } } }")

        expect(result['errors']).to be_nil
        expect(result.dig('data', field, 'nodes')).to be_empty
      end
    end
  end
end
