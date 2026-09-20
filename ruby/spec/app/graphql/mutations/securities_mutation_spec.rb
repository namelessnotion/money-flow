# frozen_string_literal: true

require 'spec_helper'

# The GraphQL layer's own job: turning arguments into a service call, and a
# refusal into an error the caller can act on.
#
# The services are stubbed rather than their Twirp clients, the way the ACH
# mutation specs do it. A mutation builds its service with a default gateway,
# so stubbing only the client would leave the mutation talking to whatever
# TRANSACTION_SERVICE_URL points at — which is a real Go server, and a passing
# spec that proves nothing about this layer. What each service does with the
# call is its own spec's business.
RSpec.describe Mutations::SecuritiesMutation do
  let(:world) { securities_world(principal_minor_units: 100_000) }

  def execute(query, variables = {})
    MoneyFlowSchema.execute(query, variables: variables).to_h
  end

  # Stands in for `service_class`, answering `result` or raising it.
  def stub_service(service_class, method: :call, result: nil, raises: nil)
    double = instance_double(service_class)
    allow(service_class).to receive(:new).and_return(double)
    if raises
      allow(double).to receive(method).and_raise(raises)
    else
      allow(double).to receive(method).and_return(result)
    end
    double
  end

  def offered(security)
    security.update(offering_transaction_id: SecureRandom.uuid_v7)
    create(:transaction_projection, aggregate_id: security.offering_transaction_id, state: 'completed')
    security
  end

  describe 'issueSecurity' do
    let(:mutation) do
      <<~GRAPHQL
        mutation($terms: SecurityTermsInput!) {
          issueSecurity(terms: $terms) { security { id name principalMinorUnits stage } }
        }
      GRAPHQL
    end

    let(:terms) do
      { 'issuerEntityId' => world.issuer.id.to_s, 'borrowerEntityId' => world.borrower.id.to_s,
        'name' => 'Elm Street rehab', 'principalMinorUnits' => '250000',
        'annualRateBps' => 1100, 'termDays' => 180 }
    end

    it 'answers with the Security it opened, re-read with its projected state' do
      security = offered(world.security)
      stub_service(Services::Securities::IssueOffering, result: security)

      data = execute(mutation, { 'terms' => terms }).dig('data', 'issueSecurity', 'security')

      expect(data['id']).to eq(security.id)
      expect(data['stage']).to eq('OFFERING')
    end

    it 'hands the service the terms it was given' do
      service = stub_service(Services::Securities::IssueOffering, result: offered(world.security))

      execute(mutation, { 'terms' => terms })

      expect(service).to have_received(:call) do |request:|
        expect(request.name).to eq('Elm Street rehab')
        expect(request.principal_minor_units).to eq(250_000)
        expect(request.annual_rate_bps).to eq(1100)
        expect(request.issuer_entity_id).to eq(world.issuer.id)
      end
    end

    it 'turns a wrong role into an error the caller can act on' do
      stub_service(Services::Securities::IssueOffering,
                   raises: Services::Securities::WrongRole.new('entity 7 is a borrower, not issuer'))

      result = execute(mutation, { 'terms' => terms })

      expect(result['errors'].first['message']).to include('is a borrower, not issuer')
    end

    it 'lets an outage through rather than answering with it' do
      # Unavailable is not a refusal the caller can act on; it is an outage,
      # and turning it into a GraphQL error would tell them the wrong thing.
      stub_service(Services::Securities::IssueOffering,
                   raises: Services::Securities::Unavailable.new('connection reset'))

      expect { execute(mutation, { 'terms' => terms }) }.to raise_error(Services::Securities::Unavailable)
    end
  end

  describe 'purchaseSecurity' do
    let(:mutation) do
      <<~GRAPHQL
        mutation($securityId: ID!, $investorEntityId: ID!, $amountMinorUnits: BigInt!) {
          purchaseSecurity(securityId: $securityId, investorEntityId: $investorEntityId,
                           amountMinorUnits: $amountMinorUnits) {
            subscription { id amountMinorUnits currency state claimLegState investor { id } }
          }
        }
      GRAPHQL
    end

    let(:variables) do
      { 'securityId' => world.security.id, 'investorEntityId' => world.investor.id.to_s,
        'amountMinorUnits' => '25000' }
    end

    it 'answers with the Subscription it recorded' do
      subscription = create(:subscription, security: world.security, investor: world.investor,
                                           amount_minor_units: 25_000)
      stub_service(Services::Securities::Purchase, result: subscription)

      data = execute(mutation, variables).dig('data', 'purchaseSecurity', 'subscription')

      expect(data['amountMinorUnits']).to eq('25000')
      expect(data['currency']).to eq('USD')
      expect(data.dig('investor', 'id')).to eq(world.investor.id.to_s)
    end

    it 'answers with null leg states until the projection has seen them' do
      subscription = create(:subscription, security: world.security, investor: world.investor)
      stub_service(Services::Securities::Purchase, result: subscription)

      data = execute(mutation, variables).dig('data', 'purchaseSecurity', 'subscription')

      expect(data['state']).to be_nil
      expect(data['claimLegState']).to be_nil
    end

    it "turns Go's oversubscription refusal into an error carrying its reason" do
      stub_service(Services::Securities::Purchase,
                   raises: Services::Securities::Refused.new(
                     'refused before any money moved: wallet "supply" has insufficient Token capacity: ' \
                     '2500 USD short'
                   ))

      expect(execute(mutation, variables)['errors'].first['message'])
        .to include('insufficient Token capacity')
    end

    it 'turns an oversubscription Ruby saw coming into an error' do
      stub_service(Services::Securities::Purchase,
                   raises: Services::Securities::Oversubscribed.new('has 1000 left'))

      expect(execute(mutation, variables)['errors'].first['message']).to include('has 1000 left')
    end
  end

  describe 'drawSecurity' do
    let(:mutation) do
      <<~GRAPHQL
        mutation($securityId: ID!) {
          drawSecurity(securityId: $securityId) { security { id stage drawState } }
        }
      GRAPHQL
    end

    it 'turns a Security that is not fully subscribed into an error' do
      stub_service(Services::Securities::Draw,
                   raises: Services::Securities::NotDrawable.new(
                     'only a fully subscribed Security draws'
                   ))

      result = execute(mutation, { 'securityId' => world.security.id })

      expect(result['errors'].first['message']).to include('only a fully subscribed Security draws')
    end
  end

  describe 'recordSecurityRepayment' do
    let(:mutation) do
      <<~GRAPHQL
        mutation($securityId: ID!, $principalMinorUnits: BigInt!, $interestMinorUnits: BigInt!) {
          recordSecurityRepayment(securityId: $securityId, principalMinorUnits: $principalMinorUnits,
                                  interestMinorUnits: $interestMinorUnits) {
            repayment { id principalMinorUnits interestMinorUnits asOf }
          }
        }
      GRAPHQL
    end

    let(:variables) do
      { 'securityId' => world.security.id, 'principalMinorUnits' => '100000',
        'interestMinorUnits' => '10000' }
    end

    it 'answers with the Repayment it recorded' do
      repayment = create(:repayment, security: world.security, principal_minor_units: 100_000,
                                     interest_minor_units: 10_000)
      stub_service(Services::Securities::RecordRepayment, result: repayment)

      data = execute(mutation, variables).dig('data', 'recordSecurityRepayment', 'repayment')

      expect(data['principalMinorUnits']).to eq('100000')
      expect(data['interestMinorUnits']).to eq('10000')
    end

    it 'turns a repayment beyond what is owed into an error' do
      stub_service(Services::Securities::RecordRepayment,
                   raises: Services::Securities::NotRepayable.new('has 100000 principal outstanding'))

      expect(execute(mutation, variables)['errors'].first['message'])
        .to include('100000 principal outstanding')
    end
  end

  describe 'disburseRepaymentNow' do
    let(:mutation) do
      <<~GRAPHQL
        mutation($repaymentId: ID!) {
          disburseRepaymentNow(repaymentId: $repaymentId) {
            repayment { id disbursements { principalMinorUnits interestMinorUnits investor { id } } }
          }
        }
      GRAPHQL
    end

    it 'answers with the Repayment and what each holder was paid' do
      repayment = create(:repayment, security: world.security)
      create(:disbursement, repayment: repayment, investor: world.investor,
                            principal_minor_units: 40_000, interest_minor_units: 4_000)
      stub_service(Services::Securities::DisburseDue,
                   result: Services::Securities::DisburseDue::Result.new(disbursed: [], failed: {}))

      data = execute(mutation, { 'repaymentId' => repayment.id })
             .dig('data', 'disburseRepaymentNow', 'repayment', 'disbursements')

      expect(data.size).to eq(1)
      expect(data.first['principalMinorUnits']).to eq('40000')
      expect(data.first.dig('investor', 'id')).to eq(world.investor.id.to_s)
    end

    it 'sweeps only the Repayment it was asked about' do
      repayment = create(:repayment, security: world.security)
      sweep = stub_service(Services::Securities::DisburseDue,
                           result: Services::Securities::DisburseDue::Result.new(disbursed: [], failed: {}))

      execute(mutation, { 'repaymentId' => repayment.id })

      expect(sweep).to have_received(:call).with(repayment_id: repayment.id)
    end

    it 'turns an unknown repayment into an error' do
      result = execute(mutation, { 'repaymentId' => SecureRandom.uuid_v7 })

      expect(result['errors'].first['message']).to include('no repayment')
    end
  end
end
