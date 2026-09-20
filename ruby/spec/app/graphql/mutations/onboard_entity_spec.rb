# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Mutations::OnboardEntity do
  let(:mutation) do
    <<~GRAPHQL
      mutation($name: String!, $role: EntityRole!) {
        onboardEntity(name: $name, role: $role) {
          entity {
            id
            name
            holderUuid
          }
        }
      }
    GRAPHQL
  end

  def execute(name:, role: 'INVESTOR')
    MoneyFlowSchema.execute(mutation, variables: { name: name, role: role }).to_h
  end

  context 'when onboarding succeeds' do
    let(:holder_uuid) { SecureRandom.uuid_v7 }
    let(:entity) { create(:entity, name: 'Test Entity', holder_uuid: holder_uuid) }
    let(:service_double) { instance_double(Services::OnboardEntity) }

    before do
      allow(Services::OnboardEntity).to receive(:new).and_return(service_double)
      allow(service_double).to receive(:call).and_return(
        OnboardEntityResponse.new(success: true, entity_id: entity.id, holder_uuid: holder_uuid)
      )
    end

    it 'returns the onboarded entity' do
      result = execute(name: 'Test Entity')

      data = result['data']['onboardEntity']['entity']
      expect(result['errors']).to be_nil
      expect(data['id']).to eq(entity.id.to_s)
      expect(data['name']).to eq('Test Entity')
      expect(data['holderUuid']).to eq(holder_uuid)
    end

    it 'forwards the requested name and role to the service as an OnboardEntityRequest' do
      execute(name: 'Test Entity', role: 'ISSUER')

      expect(service_double).to have_received(:call) do |request:|
        expect(request).to be_a(OnboardEntityRequest)
        expect(request.name).to eq('Test Entity')
        expect(request.role).to eq(Types::Enums::EntityRole::Issuer)
      end
    end

    it 'refuses a role the market has no part for, without reaching the service' do
      result = MoneyFlowSchema.execute(mutation, variables: { name: 'Test Entity', role: 'LANDLORD' }).to_h

      expect(result['errors'].first['message']).to include('$role of type EntityRole!')
      expect(service_double).not_to have_received(:call)
    end
  end

  context 'when the service raises' do
    let(:service_double) { instance_double(Services::OnboardEntity) }

    before do
      allow(Services::OnboardEntity).to receive(:new).and_return(service_double)
      allow(service_double).to receive(:call)
        .and_raise(Services::OnboardEntity::ProvisioningFailed, 'wallet taken')
    end

    it 'propagates the failure as a top-level GraphQL error' do
      expect { execute(name: 'Test Entity') }
        .to raise_error(Services::OnboardEntity::ProvisioningFailed, 'wallet taken')
    end
  end

  context 'when the onboarded entity cannot be found afterwards' do
    let(:service_double) { instance_double(Services::OnboardEntity) }

    before do
      allow(Services::OnboardEntity).to receive(:new).and_return(service_double)
      allow(service_double).to receive(:call).and_return(
        OnboardEntityResponse.new(success: true, entity_id: -1, holder_uuid: SecureRandom.uuid_v7)
      )
    end

    it 'raises rather than returning a nil entity' do
      expect { execute(name: 'Test Entity') }
        .to raise_error(/entity -1 not found after onboarding/)
    end
  end
end
